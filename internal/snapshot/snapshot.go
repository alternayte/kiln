// Package snapshot serves guest memory pages while a sandbox is restored.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// PageFaultSource serves guest memory pages during a restore. The local
// implementation runs one kiln snapfault process per restoring microVM, so Go
// garbage collection stays out of the page-fault path. A future implementation
// may read pages from object storage.
type PageFaultSource interface {
	// Start serves memPath at socketPath and returns once that socket is
	// ready for Firecracker to connect.
	Start(ctx context.Context, memPath, socketPath string) error
	// Stop stops the source. It is idempotent.
	Stop() error
}

// Process is a PageFaultSource that runs the snapfault subcommand. It also
// carries a process that outlived the daemon, adopted by the reconciler.
type Process struct {
	// Binary is the kiln executable.
	Binary string
	// PID is the adopted snapfault process. A handle built by Start uses cmd
	// instead.
	PID int

	cmd  *exec.Cmd
	done chan struct{}
}

// Adopt returns a handle to a snapfault process that survived a daemon
// restart. The pages it serves are still registered with its microVM.
func Adopt(pid int) *Process {
	return &Process{PID: pid}
}

// Start launches snapfault and waits until its socket appears.
func (p *Process) Start(ctx context.Context, memPath, socketPath string) error {
	if p.Binary == "" {
		return errors.New("snapshot: no kiln binary")
	}
	if _, err := os.Stat(memPath); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return err
	}
	_ = os.Remove(socketPath)
	// The process outlives this call and every context derived from one
	// request: the sandbox owns it until Stop.
	cmd := exec.Command(p.Binary, "snapfault", "--mem", memPath, "--sock", socketPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("snapshot: start snapfault: %w", err)
	}
	p.cmd = cmd
	p.done = make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			return nil
		}
		select {
		case <-p.done:
			return errors.New("snapshot: snapfault exited before it listened")
		case <-ctx.Done():
			_ = p.Stop()
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = p.Stop()
			return fmt.Errorf("snapshot: snapfault did not listen on %s", socketPath)
		}
	}
}

// Stop stops the process. It is idempotent.
func (p *Process) Stop() error {
	if p.PID > 0 {
		return killPID(p.PID)
	}
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	select {
	case <-p.done:
		return nil
	default:
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	return nil
}

// killPID sends SIGKILL and waits until the process is gone.
func killPID(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			return fmt.Errorf("snapshot: process %d did not exit", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// processAlive reports whether a process exists and is not a zombie.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return errors.Is(err, syscall.EPERM)
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := strings.LastIndex(string(b), ")")
	if i < 0 {
		return true
	}
	fields := strings.Fields(string(b)[i+1:])
	return len(fields) == 0 || fields[0] != "Z"
}
