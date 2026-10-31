//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alternayte/kiln/internal/guestproto"
	"golang.org/x/sys/unix"
)

// serviceName is the shape the host accepts. The guest checks it again, because
// the name becomes a path.
var serviceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// service is one image this guest runs in its own root.
type service struct {
	name     string
	cmd      *exec.Cmd
	tail     *ring
	exited   chan struct{}
	exitCode int
}

var (
	servicesMu sync.Mutex
	services   = map[string]*service{}
)

func readImageConfig(path string) (guestproto.ImageConfig, error) {
	var config guestproto.ImageConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return config, fmt.Errorf("image config: %w", err)
	}
	if err := json.Unmarshal(b, &config); err != nil {
		return config, fmt.Errorf("image config: %w", err)
	}
	return config, nil
}

// applyImageUser sets the credential from the USER of an image, read from the
// files of root. A process that already has a credential keeps it, because a
// template user wins over the image. A number with no passwd entry stays a
// number, because an image may name a uid it never declared.
func applyImageUser(attr *syscall.SysProcAttr, spec, root string) error {
	if spec == "" || attr.Credential != nil {
		return nil
	}
	name, group, _ := strings.Cut(spec, ":")
	passwd := filepath.Join(root, strings.TrimPrefix(passwdPath, "/"))
	groups := filepath.Join(root, strings.TrimPrefix(groupPath, "/"))
	if root == "" {
		passwd, groups = passwdPath, groupPath
	}
	credential := &syscall.Credential{}
	if u, err := lookupUser(name, passwd, groups); err == nil {
		credential.Uid, credential.Gid, credential.Groups = u.uid, u.gid, u.groups
	} else if uid, numErr := strconv.ParseUint(name, 10, 32); numErr == nil {
		credential.Uid, credential.Gid = uint32(uid), uint32(uid)
	} else {
		return err
	}
	if group != "" {
		gid, err := lookupGroup(group, groups)
		if err != nil {
			return fmt.Errorf("user %q: %w", spec, err)
		}
		credential.Gid = gid
	}
	attr.Credential = credential
	return nil
}

// lookupGroup finds a group by name or number.
func lookupGroup(spec, path string) (uint32, error) {
	if gid, err := strconv.ParseUint(spec, 10, 32); err == nil {
		return uint32(gid), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= 3 && fields[0] == spec {
			gid, err := strconv.ParseUint(fields[2], 10, 32)
			return uint32(gid), err
		}
	}
	return 0, fmt.Errorf("group %q is not in the image", spec)
}

// prepareRoot gives one service root the filesystems a process expects. The
// guest has already pivoted onto the writable drive.
func prepareRoot(root string) error {
	// Postgres and others keep shared memory here.
	if err := os.MkdirAll("/dev/shm", 0o1777); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", "/dev/shm", "tmpfs", 0, ""); err != nil && !errors.Is(err, unix.EBUSY) {
		return fmt.Errorf("/dev/shm: %w", err)
	}
	// devtmpfs has none of these links, and a stock image's scripts read them:
	// bash process substitution opens /dev/fd/63.
	for link, target := range map[string]string{
		"/dev/fd": "/proc/self/fd", "/dev/stdin": "/proc/self/fd/0",
		"/dev/stdout": "/proc/self/fd/1", "/dev/stderr": "/proc/self/fd/2",
	} {
		if err := os.Symlink(target, link); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s: %w", link, err)
		}
	}
	for _, target := range []string{"proc", "sys", "dev"} {
		if err := unix.Mount("/"+target, filepath.Join(root, target), "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("bind %s: %w", target, err)
		}
	}
	if err := unix.Mount("tmpfs", filepath.Join(root, "tmp"), "tmpfs", 0, "mode=1777"); err != nil {
		return fmt.Errorf("tmp: %w", err)
	}
	return nil
}

// findInRoot resolves a command against the PATH of the image, inside its root,
// because exec would otherwise search this process's own tree.
func findInRoot(root, command string, env []string) (string, error) {
	if strings.Contains(command, "/") {
		return command, nil
	}
	path := guestproto.DefaultPath
	for _, pair := range env {
		if value, ok := strings.CutPrefix(pair, "PATH="); ok {
			path = value
		}
	}
	for _, dir := range strings.Split(path, ":") {
		candidate := filepath.Join(dir, command)
		if fi, err := os.Stat(filepath.Join(root, candidate)); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%q is not on PATH in the image", command)
}

// envMap turns the Env list of an image config into a map.
func envMap(list []string) map[string]string {
	out := make(map[string]string, len(list))
	for _, pair := range list {
		if key, value, ok := strings.Cut(pair, "="); ok {
			out[key] = value
		}
	}
	return out
}

// rootCommand builds a command that runs chrooted into a service root.
func rootCommand(ctx context.Context, root string, argv []string, config guestproto.ImageConfig, env map[string]string) (*exec.Cmd, error) {
	merged := guestproto.ExecEnv(envMap(config.Env), env)
	found, err := findInRoot(root, argv[0], merged)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, found, argv[1:]...)
	// exec.Command resolved nothing, and this path is read after the chroot.
	cmd.Path = found
	cmd.Err = nil
	cmd.Env = merged
	cmd.Dir = "/"
	if config.WorkingDir != "" {
		cmd.Dir = config.WorkingDir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: root, Setsid: true}
	if err := applyImageUser(cmd.SysProcAttr, config.User, root); err != nil {
		return nil, err
	}
	return cmd, nil
}

// runServiceStart runs one service and answers once it is ready: its port
// accepts a connection, and its ready command exits 0. A service with neither
// is ready once it starts.
func runServiceStart(conn net.Conn, req *guestproto.Request) {
	fail := func(code, format string, args ...any) {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: code, Message: fmt.Sprintf(format, args...)}})
	}
	if !serviceName.MatchString(req.Service) {
		fail(guestproto.CodeInvalid, "service name %q is not valid", req.Service)
		return
	}
	root := filepath.Join(guestproto.ServiceRoots, req.Service)
	config, err := readImageConfig(filepath.Join(guestproto.ServiceImageConfigs, req.Service+".json"))
	if err != nil {
		fail(guestproto.CodeInternal, "%v", err)
		return
	}
	argv := req.Cmd
	if len(argv) == 0 {
		argv = config.Command()
	}
	if len(argv) == 0 {
		fail(guestproto.CodeInvalid, "it has no start, and its image names no command")
		return
	}
	if err := prepareRoot(root); err != nil {
		fail(guestproto.CodeInternal, "%v", err)
		return
	}
	cmd, err := rootCommand(context.Background(), root, argv, config, req.Env)
	if err != nil {
		fail(guestproto.CodeInvalid, "%v", err)
		return
	}
	svc := &service{name: req.Service, cmd: cmd, tail: &ring{limit: startOutput}, exited: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = svc.tail, svc.tail
	if err := cmd.Start(); err != nil {
		fail(guestproto.CodeInvalid, "start %v: %v", argv, err)
		return
	}
	servicesMu.Lock()
	services[req.Service] = svc
	servicesMu.Unlock()
	go func() {
		err := cmd.Wait()
		svc.exitCode = exitCodeOf(err)
		close(svc.exited)
	}()

	deadline := time.Now().Add(time.Duration(req.TimeoutSeconds) * time.Second)
	waitFor := func(what string, ready func() bool) bool {
		for {
			if ready() {
				return true
			}
			select {
			case <-svc.exited:
				fail(guestproto.CodeInvalid, "it exited with %d before %s: %s", svc.exitCode, what, svc.tail.String())
				return false
			default:
			}
			if time.Now().After(deadline) {
				fail(guestproto.CodeInvalid, "it did not reach %s in time: %s", what, svc.tail.String())
				return false
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if req.Port > 0 {
		address := fmt.Sprintf("127.0.0.1:%d", req.Port)
		listening := func() bool {
			c, err := net.DialTimeout("tcp", address, time.Second)
			if err != nil {
				return false
			}
			c.Close()
			return true
		}
		if !waitFor(fmt.Sprintf("a listener on port %d", req.Port), listening) {
			return
		}
	}
	if len(req.Ready) > 0 {
		answers := func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			check, err := rootCommand(ctx, root, req.Ready, config, req.Env)
			if err != nil {
				return false
			}
			return check.Run() == nil
		}
		if !waitFor(fmt.Sprintf("a ready command %v that exits 0", req.Ready), answers) {
			return
		}
	}
	writeResult(conn, guestproto.Result{OK: true})
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// runServiceAwait answers when one service ends, and says how. The host holds
// this connection for the life of the sandbox.
func runServiceAwait(conn net.Conn, req *guestproto.Request) {
	servicesMu.Lock()
	svc := services[req.Service]
	servicesMu.Unlock()
	if svc == nil {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{
			Code: guestproto.CodeNotFound, Message: fmt.Sprintf("no service is named %q", req.Service),
		}})
		return
	}
	<-svc.exited
	writeResult(conn, guestproto.Result{OK: true, ExitCode: svc.exitCode, Output: svc.tail.String()})
}

// serviceStates reports each service, in name order.
func serviceStates() []guestproto.ServiceState {
	servicesMu.Lock()
	defer servicesMu.Unlock()
	out := make([]guestproto.ServiceState, 0, len(services))
	for name, svc := range services {
		state := guestproto.ServiceState{Name: name, Running: true, Output: svc.tail.String()}
		select {
		case <-svc.exited:
			state.Running, state.ExitCode = false, svc.exitCode
		default:
		}
		out = append(out, state)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
