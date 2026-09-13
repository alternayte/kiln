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
	VCPUs          int
	MemoryMiB      int
	VsockCID       uint32
	VsockPort      uint32
	// BootArgs replaces the default kernel command line when set.
	BootArgs string
}

// SnapshotFiles are the two files a full snapshot consists of.
type SnapshotFiles struct {
	StatePath string
	MemPath   string
}

// Runtime starts, pauses, resumes, snapshots and stops microVMs.
type Runtime interface {
	Start(ctx context.Context, spec Spec) (*VM, error)
	Pause(ctx context.Context, vm *VM) error
	Resume(ctx context.Context, vm *VM) error
	Snapshot(ctx context.Context, vm *VM, dir string) (SnapshotFiles, error)
	Stop(ctx context.Context, vm *VM) error
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
	return fmt.Sprintf("console=ttyS0 noapic reboot=k panic=1 pci=off nomodules root=/dev/vda %s init=/kilninit", mode)
}

// ConfigRoot is the default Kiln root.
const ConfigRoot = "/var/lib/kiln"

// SandboxDir returns the host directory holding one sandbox's files.
func SandboxDir(root, id string) string {
	return filepath.Join(root, "sandboxes", id)
}
