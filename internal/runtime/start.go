package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/alternayte/kiln/internal/guestproto"
)

// StartResult is how a started application ended.
type StartResult struct {
	ExitCode int
	// Output is the tail of what the application wrote. It is what explains
	// an exit; the terminal reads the rest inside the sandbox.
	Output string
}

// Start runs the application of a template and returns when it accepts a
// connection on its port. The command keeps running after this call, unlike
// an exec, which kills its process group when the call ends.
func (vm *VM) Start(ctx context.Context, cmd []string, port, deadlineSeconds int, env map[string]string) error {
	ac, err := vm.dialAgent(ctx)
	if err != nil {
		return err
	}
	defer ac.Close()
	if err := guestproto.WriteJSON(ac.conn, guestproto.FrameRequest, guestproto.Request{
		Op:             guestproto.OpStart,
		Cmd:            cmd,
		Env:            env,
		Port:           port,
		TimeoutSeconds: deadlineSeconds,
	}); err != nil {
		return err
	}
	typ, payload, err := guestproto.ReadFrame(ac.br)
	if err != nil {
		return err
	}
	if typ != guestproto.FrameResult {
		return fmt.Errorf("runtime: unexpected frame type %q", typ)
	}
	return resultError(payload)
}

// Await blocks until the started application ends. The caller runs it in its
// own goroutine for the life of the sandbox, so a crash reaches the event log
// without polling.
func (vm *VM) Await(ctx context.Context, port int) (StartResult, error) {
	ac, err := vm.dialAgent(ctx)
	if err != nil {
		return StartResult{}, err
	}
	defer ac.Close()
	if err := guestproto.WriteJSON(ac.conn, guestproto.FrameRequest, guestproto.Request{
		Op:   guestproto.OpAwait,
		Port: port,
	}); err != nil {
		return StartResult{}, err
	}
	typ, payload, err := guestproto.ReadFrame(ac.br)
	if err != nil {
		return StartResult{}, err
	}
	if typ != guestproto.FrameResult {
		return StartResult{}, fmt.Errorf("runtime: unexpected frame type %q", typ)
	}
	var res guestproto.Result
	if err := json.Unmarshal(payload, &res); err != nil {
		return StartResult{}, fmt.Errorf("runtime: agent result: %w", err)
	}
	// The agent carries the output in the error field, because a result has
	// one place for a message. A missing application is the only real error.
	if res.Error != nil && res.Error.Code == guestproto.CodeNotFound {
		return StartResult{}, &ExecError{Code: res.Error.Code, Message: res.Error.Message}
	}
	out := StartResult{ExitCode: res.ExitCode}
	if res.Error != nil {
		out.Output = res.Error.Message
	}
	return out, nil
}
