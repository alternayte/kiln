// Package runtime owns the microVM processes. Firecracker is the only
// implementation, and this is the only package that names it.
package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
)

// Config locates the Kiln root and the installed binaries.
type Config struct {
	Root        string
	Firecracker string
	Jailer      string
}

// Spec describes one microVM to start.
type Spec struct {
	ID             string
	KernelPath     string
	RootfsPath     string
	RootfsReadOnly bool
	// OverlayPath attaches a second writable drive as /overlay.ext4 when set.
	OverlayPath string
	VCPUs       int
	MemoryMiB   int
	VsockCID    uint32
	VsockPort   uint32
	// TAPName attaches a host TAP device as eth0 when set.
	TAPName string
	// BootArgs replaces the default kernel command line when set.
	BootArgs string
}

// RestoreSpec describes a microVM loaded from a template snapshot instead of
// booting a kernel.
type RestoreSpec struct {
	Spec
	// StatePath is the host path of the Firecracker state file.
	StatePath string
	// UffdSocket is the host path of the UFFD socket that the page fault
	// source listens on.
	UffdSocket string
}

// SnapshotFiles are the two files a full snapshot consists of.
type SnapshotFiles struct {
	StatePath string
	MemPath   string
}

// Runtime starts, pauses, resumes, snapshots and stops microVMs. A sleep
// leaves a paused VM stopped without letting it run again.
type Runtime interface {
	Start(ctx context.Context, spec Spec) (*VM, error)
	Pause(ctx context.Context, vm *VM) error
	Resume(ctx context.Context, vm *VM) error
	// Snapshot pauses the VM, writes a full snapshot into dir, and resumes it.
	Snapshot(ctx context.Context, vm *VM, dir string) (SnapshotFiles, error)
	// SnapshotPaused writes a full snapshot of an already paused VM and leaves
	// it paused. The caller resumes or stops it.
	SnapshotPaused(ctx context.Context, vm *VM, dir string) (SnapshotFiles, error)
	Stop(ctx context.Context, vm *VM) error
	// StopPaused kills an already paused VM without resuming it. The saved
	// memory image stays the truth of the sandbox.
	StopPaused(ctx context.Context, vm *VM) error
}

// ExecRequest is one command to run inside a microVM.
type ExecRequest struct {
	Cmd            []string
	Cwd            string
	Env            map[string]string
	TimeoutSeconds int
}

// ExecResult is the buffered result of one command.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	TimedOut bool
}

// ExecError is a stable error from the guest agent.
type ExecError struct {
	Code    string
	Message string
}

func (e *ExecError) Error() string { return e.Code + ": " + e.Message }

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ifnamePattern matches a Linux interface name (IFNAMSIZ includes the NUL).
var ifnamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,14}$`)

func (s Spec) validate(root string) error {
	if !idPattern.MatchString(s.ID) {
		return fmt.Errorf("invalid: id %q is not a safe identifier", s.ID)
	}
	if s.KernelPath == "" || s.RootfsPath == "" {
		return fmt.Errorf("invalid: kernel and rootfs paths are required")
	}
	if s.VCPUs < 1 || s.VCPUs > 32 {
		return fmt.Errorf("invalid: vcpus %d is outside 1..32", s.VCPUs)
	}
	if s.MemoryMiB < 1 {
		return fmt.Errorf("invalid: memory_mb %d must be positive", s.MemoryMiB)
	}
	if s.VsockCID < 3 {
		return fmt.Errorf("invalid: vsock cid %d must be at least 3", s.VsockCID)
	}
	if s.VsockPort < 1 || s.VsockPort > 65535 {
		return fmt.Errorf("invalid: vsock port %d is outside 1..65535", s.VsockPort)
	}
	if s.TAPName != "" && !ifnamePattern.MatchString(s.TAPName) {
		return fmt.Errorf("invalid: tap name %q is not a valid interface name", s.TAPName)
	}
	if root == "" {
		return fmt.Errorf("internal: empty Kiln root")
	}
	return nil
}

func (s Spec) bootArgs() string {
	if s.BootArgs != "" {
		return s.BootArgs
	}
	mode := "ro"
	if !s.RootfsReadOnly {
		mode = "rw"
	}
	// noapic and pci=off belong to the pre-ACPI Firecracker cmdline. With ACPI
	// on, the MMIO devices get their interrupts through the IOAPIC, and noapic
	// leaves them without an IRQ.
	args := fmt.Sprintf("console=ttyS0 reboot=k panic=1 nomodules root=/dev/vda %s init=/kilninit", mode)
	if s.TAPName != "" {
		args += fmt.Sprintf(" ip=%s::%s:%s::eth0:off", GuestIP, GuestGateway, GuestNetmask)
	}
	return args
}

// ConfigRoot is the default Kiln root.
const ConfigRoot = "/var/lib/kiln"

// JailUID and JailGID are the unprivileged ids the jailer drops to. TAP
// devices are owned by these ids so the jailed Firecracker can attach.
const (
	JailUID = 65534
	JailGID = 65534
)

// The fixed guest network configuration. Every Kiln VM sees the same
// addresses; only the host side differs.
const (
	GuestIP      = "172.31.0.2"
	GuestGateway = "172.31.0.1"
	GuestNetmask = "255.255.255.252"
)

// SandboxDir returns the host directory holding one sandbox's files.
func SandboxDir(root, id string) string {
	return filepath.Join(root, "sandboxes", id)
}

// RestoreUffdSocket returns the host path of the UFFD socket a restoring
// microVM connects to. Firecracker sees the same socket at /run/uffd.sock.
func RestoreUffdSocket(root, id string) string {
	return filepath.Join(SandboxDir(root, id), "jail", "firecracker", id, "root", "run", "uffd.sock")
}
