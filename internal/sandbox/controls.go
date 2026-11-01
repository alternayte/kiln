package sandbox

import (
	"context"
	"errors"
	"fmt"
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
	row, err := m.egressRow(ctx, id)
	if err != nil {
		return store.Sandbox{}, err
	}
	if err := m.checkEgressKeepsBindings(ctx, row.ID, allow); err != nil {
		return store.Sandbox{}, err
	}
	return m.applyEgress(ctx, row, allow, network.Policy{Allow: allow})
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

// EgressRetention is how long a caller reads the egress record of a destroyed
// sandbox.
const EgressRetention = 7 * 24 * time.Hour

// ObserveEgress lets any TLS name through one sandbox and records it. A
// sandbox that holds a credential must not have open egress, and a sandbox
// from before the guard cannot name what it reaches.
func (m *Manager) ObserveEgress(ctx context.Context, id string) (store.Sandbox, error) {
	unlock := m.rlockOps(id)
	defer unlock()
	m.egressMu.Lock()
	defer m.egressMu.Unlock()
	row, err := m.egressRow(ctx, id)
	if err != nil {
		return store.Sandbox{}, err
	}
	if row.SecretBearing {
		return store.Sandbox{}, fmt.Errorf("%w: a sandbox that holds env values cannot observe", store.ErrConflict)
	}
	bindings, err := m.cfg.Store.Credentials(ctx, row.ID)
	if err != nil {
		return store.Sandbox{}, err
	}
	if len(bindings) > 0 {
		return store.Sandbox{}, fmt.Errorf("%w: a sandbox that holds a credential cannot observe", store.ErrConflict)
	}
	allow, err := m.EgressOf(ctx, row)
	if err != nil {
		return store.Sandbox{}, err
	}
	return m.applyEgress(ctx, row, row.EgressAllow, network.Policy{Allow: allow, Observe: true})
}

// egressRow reads a sandbox that can take a policy change.
func (m *Manager) egressRow(ctx context.Context, id string) (store.Sandbox, error) {
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
	return row, nil
}

// applyEgress stores a policy and gives it to the live link. stored is what the
// row holds, which stays nil for a sandbox that follows its template. A change
// of mode leaves an event.
func (m *Manager) applyEgress(ctx context.Context, row store.Sandbox, stored []string, policy network.Policy) (store.Sandbox, error) {
	var att *network.Attachment
	if row.State == store.SandboxRunning {
		m.mu.Lock()
		att = m.attrs[row.ID]
		m.mu.Unlock()
		if att == nil {
			return store.Sandbox{}, store.ErrConflict
		}
		if policy.Observe && !att.HasGuard() {
			return store.Sandbox{}, fmt.Errorf("%w: this sandbox has no egress guard until its next wake", store.ErrConflict)
		}
	}
	if err := m.cfg.Store.SetSandboxEgress(ctx, row.ID, stored, policy.Observe); err != nil {
		return store.Sandbox{}, err
	}
	changed := row.EgressObserve != policy.Observe
	row.EgressAllow, row.EgressObserve = stored, policy.Observe
	if changed {
		reason := "egress observe ended"
		if policy.Observe {
			reason = "egress observe started"
		}
		m.event(ctx, row.ID, row.State, row.State, reason, m.now())
	}
	if att == nil {
		return row, nil
	}
	// A policy change keeps the credentials the row holds.
	tpl, err := m.cfg.Store.GetTemplate(ctx, row.TemplateName)
	if err != nil {
		return store.Sandbox{}, err
	}
	whole := m.PolicyOf(ctx, row, tpl)
	policy.Bindings, policy.Leaves = whole.Bindings, whole.Leaves
	if err := att.SetEgress(ctx, policy); err != nil {
		return store.Sandbox{}, err
	}
	m.touch(ctx, row)
	return row, nil
}

// EgressLog returns the egress record of one sandbox. A sandbox destroyed past
// the retention no longer has one, and a sandbox that reached nothing has an
// empty one.
func (m *Manager) EgressLog(ctx context.Context, id string) (store.EgressLog, error) {
	out, err := m.cfg.Store.EgressLog(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		row, rowErr := m.cfg.Store.GetSandbox(ctx, id)
		if rowErr != nil {
			return store.EgressLog{}, rowErr
		}
		out, err = store.EgressLog{DestroyedAt: row.DestroyedAt}, nil
	}
	if err != nil {
		return store.EgressLog{}, err
	}
	if out.DestroyedAt != nil && m.now().Sub(*out.DestroyedAt) > EgressRetention {
		return store.EgressLog{}, store.ErrNotFound
	}
	return out, nil
}

// recordEgress stores what the host saw of one sandbox. The first refusal of a
// host leaves an event, so a caller approves it live, and a repeated refusal
// cannot flood the stream.
func (m *Manager) recordEgress(id string, records []network.Record, dropped int64) {
	ctx := context.Background()
	rows := make([]store.EgressRecord, 0, len(records))
	for _, rec := range records {
		rows = append(rows, store.EgressRecord{
			Host: rec.Host, Port: rec.Port, Decision: rec.Decision, Count: rec.Count,
			BytesOut: rec.BytesOut, BytesIn: rec.BytesIn,
			FirstSeen: time.Unix(rec.First, 0).UTC(), LastSeen: time.Unix(rec.Last, 0).UTC(),
		})
	}
	tenant, first, err := m.cfg.Store.RecordEgress(ctx, id, rows, dropped)
	if err != nil {
		log.Printf("sandbox: %s: egress record: %v", id, err)
		return
	}
	if tenant == "" {
		return
	}
	ctx = store.WithTenant(ctx, tenant)
	for _, host := range first {
		m.event(ctx, id, "", "", EventEgressDenied+" "+host, m.now())
	}
}

// EventEgressDenied starts the reason of the event for a first refusal.
const EventEgressDenied = "egress.denied"

// egressGuardExited records that a guard died. Egress stays closed until the
// network manager starts another.
func (m *Manager) egressGuardExited(id string) {
	ctx := context.Background()
	tenant, _, err := m.cfg.Store.RecordEgress(ctx, id, nil, 0)
	if err != nil {
		log.Printf("sandbox: %s: egress guard exit: %v", id, err)
		return
	}
	if tenant != "" {
		m.event(store.WithTenant(ctx, tenant), id, "", "", "egress guard exited", m.now())
	}
}
