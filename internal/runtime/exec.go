package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/alternayte/kiln/internal/guestproto"
)

// Exec runs one command in the VM and buffers its output. One call opens one
// agent connection and one guest process group.
func (vm *VM) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	creq := guestproto.Request{
		Op:             guestproto.OpExec,
		Cmd:            req.Cmd,
		Cwd:            req.Cwd,
		Env:            req.Env,
		TimeoutSeconds: req.TimeoutSeconds,
	}
	if e := guestproto.NormalizeExec(&creq); e != nil {
		return ExecResult{}, &ExecError{Code: e.Code, Message: e.Message}
	}
	ac, err := vm.dialAgent(ctx)
	if err != nil {
		return ExecResult{}, err
	}
	defer ac.Close()
	if err := guestproto.WriteJSON(ac.conn, guestproto.FrameRequest, creq); err != nil {
		return ExecResult{}, err
	}
	var stdout, stderr strings.Builder
	for {
		typ, payload, err := guestproto.ReadFrame(ac.br)
		if err != nil {
			return ExecResult{}, err
		}
		switch typ {
		case guestproto.FrameStdout:
			stdout.Write(payload)
		case guestproto.FrameStderr:
			stderr.Write(payload)
		case guestproto.FrameResult:
			var res guestproto.Result
			if err := json.Unmarshal(payload, &res); err != nil {
				return ExecResult{}, fmt.Errorf("runtime: agent result: %w", err)
			}
			if res.Error != nil {
				return ExecResult{}, &ExecError{Code: res.Error.Code, Message: res.Error.Message}
			}
			return ExecResult{
				ExitCode: res.ExitCode,
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				TimedOut: res.TimedOut,
			}, nil
		default:
			return ExecResult{}, fmt.Errorf("runtime: unexpected frame type %q", typ)
		}
	}
}

// IsExecError reports whether err is a stable agent error with the given code.
func IsExecError(err error, code string) bool {
	var ee *ExecError
	return errors.As(err, &ee) && ee.Code == code
}
