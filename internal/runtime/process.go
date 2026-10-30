package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/alternayte/kiln/internal/guestproto"
)

// ProcessRequest is one detached process to start.
type ProcessRequest struct {
	Cmd []string
	Cwd string
	Env map[string]string
}

// ProcessOutput is one read of a detached process.
type ProcessOutput struct {
	guestproto.ProcessState
	Stdout string
	Stderr string
}

// ask sends one request, collects the output frames, and returns the result.
func (vm *VM) ask(ctx context.Context, req guestproto.Request) (guestproto.Result, string, string, error) {
	ac, err := vm.dialAgent(ctx)
	if err != nil {
		return guestproto.Result{}, "", "", err
	}
	defer ac.Close()
	if err := guestproto.WriteJSON(ac.conn, guestproto.FrameRequest, req); err != nil {
		return guestproto.Result{}, "", "", err
	}
	var stdout, stderr strings.Builder
	for {
		typ, payload, err := guestproto.ReadFrame(ac.br)
		if err != nil {
			return guestproto.Result{}, "", "", err
		}
		switch typ {
		case guestproto.FrameStdout:
			stdout.Write(payload)
		case guestproto.FrameStderr:
			stderr.Write(payload)
		case guestproto.FrameResult:
			var res guestproto.Result
			if err := json.Unmarshal(payload, &res); err != nil {
				return guestproto.Result{}, "", "", fmt.Errorf("runtime: agent result: %w", err)
			}
			if res.Error != nil {
				return guestproto.Result{}, "", "", &ExecError{Code: res.Error.Code, Message: res.Error.Message}
			}
			return res, stdout.String(), stderr.String(), nil
		default:
			return guestproto.Result{}, "", "", fmt.Errorf("runtime: unexpected frame type %q", typ)
		}
	}
}

// CheckUser reports whether the image in this VM holds a user.
func (vm *VM) CheckUser(ctx context.Context, user string) error {
	_, _, _, err := vm.ask(ctx, guestproto.Request{Op: guestproto.OpCheckUser, User: user})
	return err
}

// StartProcess starts a detached process and returns its id once it runs.
func (vm *VM) StartProcess(ctx context.Context, req ProcessRequest) (string, error) {
	res, _, _, err := vm.ask(ctx, guestproto.Request{
		Op: guestproto.OpProcessStart, Cmd: req.Cmd, Cwd: req.Cwd, Env: req.Env,
	})
	if err != nil {
		return "", err
	}
	if res.Process == nil {
		return "", fmt.Errorf("runtime: agent started a process and named none")
	}
	return res.Process.ID, nil
}

// ReadProcess returns the state of a detached process and its output after
// each offset.
func (vm *VM) ReadProcess(ctx context.Context, id string, stdoutOffset, stderrOffset int64) (ProcessOutput, error) {
	res, stdout, stderr, err := vm.ask(ctx, guestproto.Request{
		Op: guestproto.OpProcessRead, Process: id, StdoutOffset: stdoutOffset, StderrOffset: stderrOffset,
	})
	if err != nil {
		return ProcessOutput{}, err
	}
	if res.Process == nil {
		return ProcessOutput{}, fmt.Errorf("runtime: agent read a process and reported none")
	}
	return ProcessOutput{ProcessState: *res.Process, Stdout: stdout, Stderr: stderr}, nil
}

// SignalProcess sends a signal to the process group of a detached process.
func (vm *VM) SignalProcess(ctx context.Context, id, signal string) error {
	_, _, _, err := vm.ask(ctx, guestproto.Request{Op: guestproto.OpProcessSignal, Process: id, Signal: signal})
	return err
}

// RunningProcesses counts the detached processes that run now.
func (vm *VM) RunningProcesses(ctx context.Context) (int, error) {
	res, _, _, err := vm.ask(ctx, guestproto.Request{Op: guestproto.OpProcessCount})
	if err != nil {
		return 0, err
	}
	return res.Running, nil
}
