package sandbox

import (
	"context"
	"log"
	"time"

	"github.com/alternayte/kiln/internal/guestproto"
	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/store"
)

// processCountTimeout bounds the question the idle deadline asks the guest.
const processCountTimeout = 10 * time.Second

// SetEgress replaces the egress policy of one sandbox. A sleeping sandbox
// takes the change without a wake, and the wake applies it. The shared lock
// is enough, because a sleep and a wake hold the exclusive side, and an exec
// in flight must not delay a policy change.
func (m *Manager) SetEgress(ctx context.Context, id string, allow []string) (store.Sandbox, error) {
	if allow == nil {
		allow = []string{}
	}
	if err := network.ValidateAllow(allow); err != nil {
		return store.Sandbox{}, invalidf("%v", err)
	}
	unlock := m.rlockOps(id)
	defer unlock()
	m.egressMu.Lock()
	defer m.egressMu.Unlock()
	row, err := m.cfg.Store.GetSandbox(ctx, id)
	if err != nil {
		return store.Sandbox{}, err
	}
	switch {
	case row.DestroyedAt != nil || row.State == store.SandboxDestroyed:
		return store.Sandbox{}, store.ErrNotFound
	case row.State != store.SandboxRunning && row.State != store.SandboxSleeping:
		return store.Sandbox{}, store.ErrConflict
	}
	if err := m.cfg.Store.SetSandboxEgress(ctx, id, allow); err != nil {
		return store.Sandbox{}, err
	}
	row.EgressAllow = allow
	if row.State == store.SandboxSleeping {
		return row, nil
	}
	m.mu.Lock()
	att := m.attrs[id]
	m.mu.Unlock()
	if att == nil {
		return store.Sandbox{}, store.ErrConflict
	}
	if err := att.SetEgress(ctx, allow); err != nil {
		return store.Sandbox{}, err
	}
	m.touch(ctx, row)
	return row, nil
}

// EgressOf returns the policy in force on one sandbox row.
func (m *Manager) EgressOf(ctx context.Context, row store.Sandbox) ([]string, error) {
	if row.EgressAllow != nil {
		return row.EgressAllow, nil
	}
	tpl, err := m.cfg.Store.GetTemplate(ctx, row.TemplateName)
	if err != nil {
		return nil, err
	}
	return tpl.EgressAllow, nil
}

// StartProcess starts a detached process. A sleeping sandbox wakes first.
func (m *Manager) StartProcess(ctx context.Context, id string, req runtime.ProcessRequest) (string, error) {
	var pid string
	err := m.withVM(ctx, id, func(vm *runtime.VM) (err error) {
		pid, err = vm.StartProcess(ctx, req)
		return err
	})
	return pid, err
}

// ReadProcess reads the state and the output of one detached process. A poll
// is a caller at work, so it counts as activity.
func (m *Manager) ReadProcess(ctx context.Context, id, pid string, stdoutOffset, stderrOffset int64) (runtime.ProcessOutput, error) {
	var out runtime.ProcessOutput
	err := m.withVM(ctx, id, func(vm *runtime.VM) (err error) {
		out, err = vm.ReadProcess(ctx, pid, stdoutOffset, stderrOffset)
		return err
	})
	return out, err
}

// SignalProcess signals the process group of one detached process.
func (m *Manager) SignalProcess(ctx context.Context, id, pid, signal string) error {
	return m.withVM(ctx, id, func(vm *runtime.VM) error {
		return vm.SignalProcess(ctx, pid, signal)
	})
}

// withVM runs one guest call under the shared lifecycle lock and records the
// activity when it succeeds.
func (m *Manager) withVM(ctx context.Context, id string, call func(*runtime.VM) error) error {
	row, unlock, err := m.acquireRunning(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	vm, err := m.vmFor(row)
	if err != nil {
		return err
	}
	if err := call(vm); err != nil {
		return err
	}
	m.touch(ctx, row)
	return nil
}

// processesRunning asks the guest whether a detached process runs. The guest
// is the only true source: it survives a daemon restart, and a process can
// end with nobody watching. A guest built before detached processes refuses
// the question, and a guest that cannot answer holds nothing to protect.
func (m *Manager) processesRunning(ctx context.Context, row store.Sandbox) bool {
	vm, err := m.vmFor(row)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, processCountTimeout)
	defer cancel()
	running, err := vm.RunningProcesses(ctx)
	if err != nil {
		if !runtime.IsExecError(err, guestproto.CodeInvalid) {
			log.Printf("sandbox: %s: process count: %v", row.ID, err)
		}
		return false
	}
	return running > 0
}
