package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alternayte/kiln/internal/guestproto"
)

const (
	cgroupRoot          = "/sys/fs/cgroup"
	kvmDevice           = "/dev/kvm"
	apiSocketTimeout    = 5 * time.Second
	agentReadyTimeout   = 20 * time.Second
	handshakeTimeout    = 5 * time.Second
	stopGrace           = 5 * time.Second
	agentPollInterval   = 100 * time.Millisecond
	shutdownWaitTimeout = 2 * time.Second
)

// Firecracker is the runtime implementation. Every VM runs under jailer.
type Firecracker struct {
	root        string
	firecracker string
	jailer      string
}

// New returns the Firecracker runtime. Config paths default to the Kiln root.
func New(cfg Config) (*Firecracker, error) {
	if cfg.Root == "" {
		cfg.Root = ConfigRoot
	}
	if cfg.Firecracker == "" {
		cfg.Firecracker = filepath.Join(cfg.Root, "bin", "firecracker")
	}
	if cfg.Jailer == "" {
		cfg.Jailer = filepath.Join(cfg.Root, "bin", "jailer")
	}
	for _, p := range []string{cfg.Firecracker, cfg.Jailer} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("runtime: %w", err)
		}
	}
	return &Firecracker{root: cfg.Root, firecracker: cfg.Firecracker, jailer: cfg.Jailer}, nil
}

// VM is one running microVM.
type VM struct {
	ID          string
	PID         int
	Dir         string
	Socket      string
	VsockSocket string
	VsockPort   uint32

	api     *api
	cmd     *exec.Cmd
	console *os.File
	done    chan struct{}
	waitErr error
}

func (vm *VM) chrootDir() string {
	return filepath.Join(vm.Dir, "jail", "firecracker", vm.ID, "root")
}

// Restore loads a snapshot into a new microVM and waits until the guest agent
// answers a fresh hello. The VM resumes before the call returns.
func (f *Firecracker) Restore(ctx context.Context, spec RestoreSpec) (*VM, error) {
	if err := spec.validate(f.root); err != nil {
		return nil, err
	}
	if spec.StatePath == "" || spec.UffdSocket == "" {
		return nil, fmt.Errorf("invalid: state path and uffd socket are required")
	}
	if err := checkKVM(); err != nil {
		return nil, err
	}
	vm := &VM{
		ID:        spec.ID,
		Dir:       SandboxDir(f.root, spec.ID),
		VsockPort: spec.VsockPort,
		done:      make(chan struct{}),
	}
	vm.Socket = filepath.Join(vm.chrootDir(), "run", "firecracker.sock")
	vm.VsockSocket = filepath.Join(vm.chrootDir(), "run", "vsock.sock")

	if err := f.prepare(spec.Spec, vm); err != nil {
		return nil, f.fail(vm, err)
	}
	// The state file is copied, never linked. Firecracker truncates the file at
	// this path when it creates a new snapshot, and a hard link would write
	// through to the image the sandbox was restored from. The copy belongs to
	// the jailer's user so the next snapshot can replace it.
	statePath := filepath.Join(vm.chrootDir(), "snapshots", "state")
	if err := copyFile(spec.StatePath, statePath); err != nil {
		return nil, f.fail(vm, err)
	}
	if err := os.Chown(statePath, JailUID, JailGID); err != nil {
		return nil, f.fail(vm, err)
	}
	if err := f.launch(spec.Spec, vm); err != nil {
		return nil, f.fail(vm, err)
	}
	if err := f.loadSnapshot(ctx, spec, vm); err != nil {
		return nil, f.fail(vm, err)
	}
	if err := f.waitAgent(ctx, vm); err != nil {
		return nil, f.fail(vm, err)
	}
	return vm, nil
}

// loadSnapshot points Firecracker at the state file and the UFFD socket, then
// resumes the loaded VM. Every other device comes from the snapshot.
func (f *Firecracker) loadSnapshot(ctx context.Context, spec RestoreSpec, vm *VM) error {
	if err := waitSocket(ctx, vm); err != nil {
		return err
	}
	vm.api = newAPI(vm.Socket)
	load := map[string]any{
		"snapshot_path":     "/snapshots/state",
		"mem_backend":       map[string]any{"backend_path": "/run/uffd.sock", "backend_type": "Uffd"},
		"resume_vm":         false,
		"track_dirty_pages": false,
	}
	if spec.TAPName != "" {
		load["network_overrides"] = []map[string]string{
			{"iface_id": "eth0", "host_dev_name": spec.TAPName},
		}
	}
	if err := vm.api.put(ctx, "/snapshot/load", load); err != nil {
		return err
	}
	return vm.api.patch(ctx, "/vm", map[string]string{"state": "Resumed"})
}

// Start boots a microVM and waits until the guest agent answers.
func (f *Firecracker) Start(ctx context.Context, spec Spec) (*VM, error) {
	if err := spec.validate(f.root); err != nil {
		return nil, err
	}
	if err := checkKVM(); err != nil {
		return nil, err
	}
	vm := &VM{
		ID:        spec.ID,
		Dir:       SandboxDir(f.root, spec.ID),
		VsockPort: spec.VsockPort,
		done:      make(chan struct{}),
	}
	vm.Socket = filepath.Join(vm.chrootDir(), "run", "firecracker.sock")
	vm.VsockSocket = filepath.Join(vm.chrootDir(), "run", "vsock.sock")

	if err := f.prepare(spec, vm); err != nil {
		return nil, f.fail(vm, err)
	}
	if err := f.launch(spec, vm); err != nil {
		return nil, f.fail(vm, err)
	}
	if err := f.configure(ctx, spec, vm); err != nil {
		return nil, f.fail(vm, err)
	}
	if err := f.waitAgent(ctx, vm); err != nil {
		return nil, f.fail(vm, err)
	}
	return vm, nil
}

// Adopt reattaches to a Firecracker process from a previous daemon. The jail
// directory and both sockets must still exist, and the process must answer on
// its API socket. The guest keeps running.
func (f *Firecracker) Adopt(ctx context.Context, spec AdoptSpec) (*VM, error) {
	if !idPattern.MatchString(spec.ID) {
		return nil, fmt.Errorf("invalid: id %q is not a safe identifier", spec.ID)
	}
	if spec.PID <= 0 || !processAlive(spec.PID) {
		return nil, fmt.Errorf("runtime: no process %d for sandbox %s", spec.PID, spec.ID)
	}
	vm := &VM{
		ID:        spec.ID,
		PID:       spec.PID,
		Dir:       SandboxDir(f.root, spec.ID),
		VsockPort: guestproto.Port,
	}
	vm.Socket = filepath.Join(vm.chrootDir(), "run", "firecracker.sock")
	vm.VsockSocket = filepath.Join(vm.chrootDir(), "run", "vsock.sock")
	for _, path := range []string{vm.Socket, vm.VsockSocket} {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("runtime: adopt %s: %w", spec.ID, err)
		}
	}
	vm.api = newAPI(vm.Socket)
	callCtx, cancel := context.WithTimeout(ctx, apiSocketTimeout)
	defer cancel()
	if _, err := vm.api.get(callCtx, "/machine-config"); err != nil {
		return nil, fmt.Errorf("runtime: adopt %s: %w", spec.ID, err)
	}
	return vm, nil
}

// Stop asks the guest to power off, waits, and removes every host resource.
func (f *Firecracker) Stop(ctx context.Context, vm *VM) error {
	if vm.alive() {
		f.askShutdown(ctx, vm)
	}
	if !vm.waitExit(ctx, stopGrace) {
		_ = KillProcess(vm.PID)
	}
	return vm.cleanup(true)
}

// StopPaused kills a paused VM without resuming it, then removes its host
// resources but keeps its directory. A sleeping sandbox uses it: the memory
// image and the writable drive stay for the wake.
func (f *Firecracker) StopPaused(ctx context.Context, vm *VM) error {
	_ = KillProcess(vm.PID)
	return vm.cleanup(false)
}

// Pause freezes the VM.
func (f *Firecracker) Pause(ctx context.Context, vm *VM) error {
	return vm.api.patch(ctx, "/vm", map[string]string{"state": "Paused"})
}

// Resume unfreezes the VM.
func (f *Firecracker) Resume(ctx context.Context, vm *VM) error {
	return vm.api.patch(ctx, "/vm", map[string]string{"state": "Resumed"})
}

// Snapshot pauses a VM, writes a full memory and state snapshot into dir, and
// resumes it.
func (f *Firecracker) Snapshot(ctx context.Context, vm *VM, dir string) (SnapshotFiles, error) {
	if err := f.Pause(ctx, vm); err != nil {
		return SnapshotFiles{}, err
	}
	files, err := f.SnapshotPaused(ctx, vm, dir)
	if resumeErr := f.Resume(ctx, vm); err == nil {
		err = resumeErr
	}
	return files, err
}

// SnapshotPaused writes a full memory and state snapshot of an already paused
// VM into dir. The VM stays paused.
func (f *Firecracker) SnapshotPaused(ctx context.Context, vm *VM, dir string) (SnapshotFiles, error) {
	out := SnapshotFiles{}
	err := vm.api.put(ctx, "/snapshot/create", map[string]any{
		"snapshot_path": "/snapshots/state",
		"mem_file_path": "/snapshots/mem",
		"snapshot_type": "Full",
	})
	if err == nil {
		err = os.MkdirAll(dir, 0o750)
	}
	if err == nil {
		out.StatePath, err = moveOut(filepath.Join(vm.chrootDir(), "snapshots", "state"), filepath.Join(dir, "state"))
	}
	if err == nil {
		out.MemPath, err = moveOut(filepath.Join(vm.chrootDir(), "snapshots", "mem"), filepath.Join(dir, "mem"))
	}
	return out, err
}

func checkKVM() error {
	f, err := os.OpenFile(kvmDevice, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("runtime: %s: %w", kvmDevice, err)
	}
	return f.Close()
}

// prepare builds the jail skeleton, links the kernel and rootfs into it, and
// makes the per-VM cgroup with the template's limits. The jailer creates
// /dev/kvm inside the jail.
func (f *Firecracker) prepare(spec Spec, vm *VM) error {
	chroot := vm.chrootDir()
	runDir := filepath.Join(chroot, "run")
	snapDir := filepath.Join(chroot, "snapshots")
	for _, d := range []string{runDir, snapDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := os.Chown(runDir, JailUID, JailGID); err != nil {
		return err
	}
	if err := os.Chown(snapDir, JailUID, JailGID); err != nil {
		return err
	}
	if err := linkOrCopy(spec.KernelPath, filepath.Join(chroot, "vmlinux")); err != nil {
		return err
	}
	rootfs := filepath.Join(chroot, "rootfs.ext4")
	if err := linkOrCopy(spec.RootfsPath, rootfs); err != nil {
		return err
	}
	if !spec.RootfsReadOnly {
		if err := os.Chown(rootfs, JailUID, JailGID); err != nil {
			return err
		}
	}
	if spec.OverlayPath != "" {
		overlay := filepath.Join(chroot, "overlay.ext4")
		if err := linkOrCopy(spec.OverlayPath, overlay); err != nil {
			return err
		}
		if err := os.Chown(overlay, JailUID, JailGID); err != nil {
			return err
		}
	}
	if err := ensureCgroupBase(); err != nil {
		return err
	}
	dir := CgroupDir(spec.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return applyCgroupLimits(dir, spec.VCPUs, spec.MemoryMiB)
}

// memoryOverheadMiB is the headroom a sandbox cgroup keeps above the guest's
// memory. Firecracker, its page tables and the faulted memory live in the same
// cgroup, so a limit equal to the guest size would kill the VM.
const memoryOverheadMiB = 64

// SandboxMemoryLimit is the cgroup memory limit of a sandbox with the given
// guest memory. The gate asserts the host value against it.
func SandboxMemoryLimit(memoryMiB int) int64 {
	return int64(memoryMiB+memoryOverheadMiB) << 20
}

// ensureCgroupBase creates the cgroup parent and enables the controllers the
// per-VM limits need. The parent holds no process, so the kernel accepts the
// write.
func ensureCgroupBase() error {
	dir := ManagerCgroupDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("runtime: cgroup base: %w", err)
	}
	path := filepath.Join(dir, "cgroup.subtree_control")
	if err := os.WriteFile(path, []byte("+cpu +memory\n"), 0o644); err != nil {
		return fmt.Errorf("runtime: enable cgroup controllers: %w", err)
	}
	return nil
}

// applyCgroupLimits sets the memory and cpu bounds of one sandbox cgroup.
func applyCgroupLimits(dir string, vcpus, memoryMiB int) error {
	memory := strconv.Itoa((memoryMiB + memoryOverheadMiB) << 20)
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(memory+"\n"), 0o644); err != nil {
		return fmt.Errorf("runtime: cgroup memory limit: %w", err)
	}
	quota := strconv.Itoa(vcpus * 100000)
	if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(quota+" 100000\n"), 0o644); err != nil {
		return fmt.Errorf("runtime: cgroup cpu limit: %w", err)
	}
	return nil
}

// launch execs jailer, which copies Firecracker into the chroot, drops
// privileges, and execs it. The child is Firecracker itself, so Wait and the
// process id refer to it.
func (f *Firecracker) launch(spec Spec, vm *VM) error {
	console, err := os.OpenFile(filepath.Join(vm.Dir, "console.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	vm.console = console
	args := []string{
		"--id", spec.ID,
		"--exec-file", f.firecracker,
		"--uid", strconv.Itoa(JailUID),
		"--gid", strconv.Itoa(JailGID),
		"--chroot-base-dir", filepath.Join(vm.Dir, "jail"),
		"--cgroup-version", "2",
		"--parent-cgroup", "kiln/" + spec.ID,
		"--",
		"--api-sock", "/run/firecracker.sock",
		"--log-path", "/run/firecracker.log",
		"--level", "Info",
	}
	cmd := exec.Command(f.jailer, args...)
	cmd.Stdout = console
	cmd.Stderr = console
	if err := cmd.Start(); err != nil {
		return err
	}
	vm.cmd = cmd
	vm.PID = cmd.Process.Pid
	go func() {
		vm.waitErr = cmd.Wait()
		close(vm.done)
	}()
	return nil
}

// configure programs Firecracker over its API and starts the microVM.
func (f *Firecracker) configure(ctx context.Context, spec Spec, vm *VM) error {
	if err := waitSocket(ctx, vm); err != nil {
		return err
	}
	vm.api = newAPI(vm.Socket)
	if err := vm.api.put(ctx, "/boot-source", map[string]any{
		"kernel_image_path": "/vmlinux",
		"boot_args":         spec.bootArgs(),
	}); err != nil {
		return err
	}
	if err := vm.api.put(ctx, "/drives/rootfs", map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   "/rootfs.ext4",
		"is_root_device": true,
		"is_read_only":   spec.RootfsReadOnly,
	}); err != nil {
		return err
	}
	if spec.OverlayPath != "" {
		if err := vm.api.put(ctx, "/drives/overlay", map[string]any{
			"drive_id":       "overlay",
			"path_on_host":   "/overlay.ext4",
			"is_root_device": false,
			"is_read_only":   false,
		}); err != nil {
			return err
		}
	}
	if err := vm.api.put(ctx, "/machine-config", map[string]any{
		"vcpu_count":   spec.VCPUs,
		"mem_size_mib": spec.MemoryMiB,
		"smt":          false,
	}); err != nil {
		return err
	}
	if spec.TAPName != "" {
		if err := vm.api.put(ctx, "/network-interfaces/eth0", map[string]any{
			"iface_id":      "eth0",
			"host_dev_name": spec.TAPName,
		}); err != nil {
			return err
		}
	}
	if err := vm.api.put(ctx, "/vsock", map[string]any{
		"guest_cid": spec.VsockCID,
		"uds_path":  "/run/vsock.sock",
	}); err != nil {
		return err
	}
	return vm.api.put(ctx, "/actions", map[string]any{"action_type": "InstanceStart"})
}

func waitSocket(ctx context.Context, vm *VM) error {
	deadline := time.Now().Add(apiSocketTimeout)
	for {
		if _, err := os.Stat(vm.Socket); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("runtime: api socket %s did not appear", vm.Socket)
		}
		select {
		case <-vm.done:
			return fmt.Errorf("runtime: firecracker exited: %v", vm.waitErr)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// waitAgent polls until the guest agent answers a hello.
func (f *Firecracker) waitAgent(ctx context.Context, vm *VM) error {
	deadline := time.Now().Add(agentReadyTimeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("runtime: guest agent did not answer within %s", agentReadyTimeout)
		}
		ac, err := vm.dialAgent(ctx)
		if err == nil {
			err = hello(ac)
			ac.Close()
			if err == nil {
				return nil
			}
		}
		select {
		case <-vm.done:
			return fmt.Errorf("runtime: firecracker exited: %v", vm.waitErr)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(agentPollInterval):
		}
	}
}

// agentConn is a host connection to one guest agent session.
type agentConn struct {
	conn net.Conn
	br   *bufio.Reader
	stop func() bool
}

func (ac *agentConn) Close() error {
	if ac.stop != nil {
		ac.stop()
	}
	return ac.conn.Close()
}

// dialAgent connects to the Firecracker vsock socket and performs the
// host-initiated handshake for the agent port.
func (vm *VM) dialAgent(ctx context.Context) (*agentConn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", vm.VsockSocket)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", vm.VsockPort); err != nil {
		stop()
		conn.Close()
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		stop()
		conn.Close()
		return nil, err
	}
	if !strings.HasPrefix(line, "OK ") {
		stop()
		conn.Close()
		return nil, fmt.Errorf("runtime: vsock handshake: %q", line)
	}
	_ = conn.SetReadDeadline(time.Time{})
	ac := &agentConn{conn: conn, br: br, stop: stop}
	return ac, nil
}

func hello(ac *agentConn) error {
	if err := guestproto.WriteJSON(ac.conn, guestproto.FrameRequest, guestproto.Request{Op: guestproto.OpHello}); err != nil {
		return err
	}
	typ, payload, err := guestproto.ReadFrame(ac.br)
	if err != nil {
		return err
	}
	if typ != guestproto.FrameResult {
		return fmt.Errorf("runtime: agent hello: frame %q", typ)
	}
	var res guestproto.Result
	if err := json.Unmarshal(payload, &res); err != nil {
		return err
	}
	if res.Error != nil {
		return res.Error
	}
	if !res.OK {
		return fmt.Errorf("runtime: agent hello: not ok")
	}
	return nil
}

// askShutdown tells the agent to power the guest off. Failure is not fatal;
// Stop kills the process if the guest does not exit.
func (f *Firecracker) askShutdown(ctx context.Context, vm *VM) {
	ctx, cancel := context.WithTimeout(ctx, shutdownWaitTimeout)
	defer cancel()
	ac, err := vm.dialAgent(ctx)
	if err != nil {
		return
	}
	defer ac.Close()
	if err := guestproto.WriteJSON(ac.conn, guestproto.FrameRequest, guestproto.Request{Op: guestproto.OpShutdown}); err != nil {
		return
	}
	_, _, _ = guestproto.ReadFrame(ac.br)
}

// cleanup removes the cgroup and, when removeDir is set, the sandbox
// directory. It is idempotent. A sleep keeps the directory.
func (vm *VM) cleanup(removeDir bool) error {
	_ = KillProcess(vm.PID)
	if vm.console != nil {
		_ = vm.console.Close()
	}
	var first error
	if removeDir {
		if err := os.RemoveAll(vm.Dir); err != nil && first == nil {
			first = err
		}
	}
	if err := removeCgroup(vm.ID); err != nil && first == nil {
		first = err
	}
	return first
}

// alive reports whether the VM process still runs.
func (vm *VM) alive() bool {
	if vm.cmd != nil {
		select {
		case <-vm.done:
			return false
		default:
			return true
		}
	}
	return processAlive(vm.PID)
}

// waitExit waits until the process is gone. It returns false on timeout.
func (vm *VM) waitExit(ctx context.Context, grace time.Duration) bool {
	if vm.cmd != nil {
		select {
		case <-vm.done:
			return true
		case <-ctx.Done():
			return false
		case <-time.After(grace):
			return false
		}
	}
	deadline := time.Now().Add(grace)
	for processAlive(vm.PID) {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// processAlive reports whether a process exists and has not become a zombie.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return errors.Is(err, syscall.EPERM)
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// The second field is the command name in parentheses, and it may contain
	// spaces. The state is the first field after the last ')'.
	i := strings.LastIndex(string(b), ")")
	if i < 0 {
		return true
	}
	fields := strings.Fields(string(b)[i+1:])
	return len(fields) == 0 || fields[0] != "Z"
}

func removeCgroup(id string) error {
	dir := CgroupDir(id)
	// Jailer creates a child cgroup named after the id. Remove it first, then
	// the parent, which is the cgroup that carries the limits.
	for _, target := range []string{filepath.Join(dir, id), dir} {
		var err error
		for i := 0; i < 20; i++ {
			err = os.Remove(target)
			if err == nil || errors.Is(err, fs.ErrNotExist) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("runtime: remove cgroup %s: %w", target, err)
		}
	}
	return nil
}

// linkOrCopy links src into the jail. A different filesystem falls back to a
// copy.
func linkOrCopy(src, dst string) error {
	err := os.Link(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return fmt.Errorf("runtime: link %s: %w", src, err)
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// moveOut moves a snapshot file out of the jail. A different filesystem falls
// back to a copy.
func moveOut(src, dst string) (string, error) {
	if err := os.Rename(src, dst); err == nil {
		return dst, nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return "", err
	}
	if err := copyFile(src, dst); err != nil {
		return "", err
	}
	if err := os.Remove(src); err != nil {
		return "", err
	}
	return dst, nil
}

func withConsole(vm *VM, err error) error {
	tail := vm.ConsoleTail()
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w; console tail: %s", err, tail)
}

// ConsoleTail returns the last lines of the VM console log, for errors. It is
// empty before the console file exists.
func (vm *VM) ConsoleTail() string {
	return consoleTail(filepath.Join(vm.Dir, "console.log"))
}

// fail attaches the console and Firecracker log tails to err, then removes the
// VM's host resources. The tails are captured before cleanup deletes them.
func (f *Firecracker) fail(vm *VM, err error) error {
	err = withConsole(vm, err)
	if tail := vm.LogTail(); tail != "" {
		err = fmt.Errorf("%w; firecracker log: %s", err, tail)
	}
	_ = vm.cleanup(true)
	return err
}

// LogTail returns the last lines of the Firecracker log, for errors.
func (vm *VM) LogTail() string {
	return consoleTail(filepath.Join(vm.chrootDir(), "run", "firecracker.log"))
}

// consoleTail returns the last few lines of a VM's console log, for errors.
func consoleTail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	const limit = 4096
	offset := info.Size() - limit
	if offset < 0 {
		offset = 0
	}
	b := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(b, offset); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return strings.TrimSpace(string(b))
}
