package sandbox

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/snapshot"
	"github.com/alternayte/kiln/internal/store"
)

// opLock is the lifecycle lock of one sandbox. refs counts the callers that
// hold or wait for it, so the table drops an entry on the last release.
type opLock struct {
	mu   sync.RWMutex
	refs int
}

// lockOps takes the exclusive lifecycle lock of one sandbox. The returned
// function releases it. A sleep, a wake or a destroy holds the exclusive
// side; exec and file requests hold the shared side.
func (m *Manager) lockOps(id string) func() {
	l := m.acquireOps(id)
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		m.releaseOps(id, l)
	}
}

// rlockOps takes the shared lifecycle lock of one sandbox.
func (m *Manager) rlockOps(id string) func() {
	l := m.acquireOps(id)
	l.mu.RLock()
	return func() {
		l.mu.RUnlock()
		m.releaseOps(id, l)
	}
}

func (m *Manager) acquireOps(id string) *opLock {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	l := m.ops[id]
	if l == nil {
		l = &opLock{}
		m.ops[id] = l
	}
	l.refs++
	return l
}

func (m *Manager) releaseOps(id string, l *opLock) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	l.refs--
	if l.refs == 0 {
		delete(m.ops, id)
	}
}

// lockedReader holds the sandbox's shared lifecycle lock until its reader is
// closed, so a sleep cannot stop the VM mid-read.
type lockedReader struct {
	io.ReadCloser
	once   sync.Once
	unlock func()
}

func (l *lockedReader) Close() error {
	err := l.ReadCloser.Close()
	l.once.Do(l.unlock)
	return err
}

// lockedTerminal holds the sandbox read lock for the life of a terminal.
type lockedTerminal struct {
	*runtime.Terminal
	once   sync.Once
	unlock func()
}

func (l *lockedTerminal) Close() error {
	err := l.Terminal.Close()
	l.once.Do(l.unlock)
	return err
}

// touch records activity and moves the idle deadline. It is the only writer
// of last_active_at after a restore.
func (m *Manager) touch(ctx context.Context, row store.Sandbox) {
	now := m.now()
	if err := m.cfg.Store.TouchSandbox(ctx, row.ID, now); err != nil {
		log.Printf("sandbox: %s: touch: %v", row.ID, err)
	}
	row.LastActiveAt = now
	m.arm(row)
}

// arm gives one sandbox its single timer. The timer fires at the earliest of
// the idle deadline and the ttl deadline; which action applies is decided
// when it fires, from the state then. It is idempotent.
func (m *Manager) arm(row store.Sandbox) {
	m.timerMu.Lock()
	defer m.timerMu.Unlock()
	if t := m.timers[row.ID]; t != nil {
		t.Stop()
		delete(m.timers, row.ID)
	}
	if row.DestroyedAt != nil {
		return
	}
	deadline, ok := nextDeadline(row)
	if !ok {
		return
	}
	delay := time.Until(deadline)
	if delay < 0 {
		delay = 0
	}
	id := row.ID
	m.timers[id] = time.AfterFunc(delay, func() { m.enforce(id) })
}

// stopTimer removes the timer of one sandbox.
func (m *Manager) stopTimer(id string) {
	m.timerMu.Lock()
	defer m.timerMu.Unlock()
	if t := m.timers[id]; t != nil {
		t.Stop()
		delete(m.timers, id)
	}
}

// nextDeadline is the next moment a sandbox must sleep or die. A sandbox in
// creating, waking or stopping is owned by the request that moved it there,
// so it has no deadline.
func nextDeadline(row store.Sandbox) (time.Time, bool) {
	var deadline time.Time
	set := false
	consider := func(t time.Time) {
		if !set || t.Before(deadline) {
			deadline = t
			set = true
		}
	}
	switch row.State {
	case store.SandboxRunning:
		consider(row.LastActiveAt.Add(time.Duration(row.IdleSeconds) * time.Second))
		if row.TTLSeconds != nil {
			consider(row.CreatedAt.Add(time.Duration(*row.TTLSeconds) * time.Second))
		}
	case store.SandboxSleeping:
		if row.TTLSeconds != nil {
			consider(row.CreatedAt.Add(time.Duration(*row.TTLSeconds) * time.Second))
		}
	}
	return deadline, set
}

// enforce applies a deadline that came due. It re-reads the row, because the
// timer may have fired while another transition ran, and it holds the
// sandbox's lifecycle lock so it never races a request.
func (m *Manager) enforce(id string) {
	unlock := m.lockOps(id)
	defer unlock()
	ctx := context.Background()
	row, err := m.cfg.Store.GetSandbox(ctx, id)
	if err != nil || row.DestroyedAt != nil || row.State == store.SandboxFailed {
		m.stopTimer(id)
		return
	}
	now := m.now()
	ttlDue := row.TTLSeconds != nil && !now.Before(row.CreatedAt.Add(time.Duration(*row.TTLSeconds)*time.Second))
	switch row.State {
	case store.SandboxRunning:
		if row.Lifecycle == store.LifecycleEphemeral {
			if ttlDue || !now.Before(row.LastActiveAt.Add(time.Duration(row.IdleSeconds)*time.Second)) {
				if err := m.destroyLocked(ctx, id); err != nil {
					log.Printf("sandbox: %s: idle destroy: %v", id, err)
				}
				return
			}
		} else {
			if ttlDue {
				if err := m.destroyLocked(ctx, id); err != nil {
					log.Printf("sandbox: %s: ttl destroy: %v", id, err)
				}
				return
			}
			if !now.Before(row.LastActiveAt.Add(time.Duration(row.IdleSeconds) * time.Second)) {
				if err := m.sleepLocked(ctx, id); err != nil {
					log.Printf("sandbox: %s: idle sleep: %v", id, err)
					m.arm(row)
				}
				return
			}
		}
	case store.SandboxSleeping:
		if ttlDue {
			if err := m.destroyLocked(ctx, id); err != nil {
				log.Printf("sandbox: %s: ttl destroy: %v", id, err)
			}
			return
		}
	}
	m.arm(row)
}

// ScheduleAll gives every live sandbox its timer. The reconciler calls it
// after a sweep, so a restarted daemon resumes idle and ttl handling.
func (m *Manager) ScheduleAll(ctx context.Context) error {
	rows, err := m.cfg.Store.ListSandboxes(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.DestroyedAt == nil && row.State != store.SandboxFailed {
			m.arm(row)
		}
	}
	return nil
}

// Sleep snapshots a running sandbox into its own directory and stops the VM.
// The sandbox keeps its id, its files and its published hostnames. It is
// idempotent on a sleeping sandbox.
func (m *Manager) Sleep(ctx context.Context, id string) error {
	unlock := m.lockOps(id)
	defer unlock()
	return m.sleepLocked(ctx, id)
}

func (m *Manager) sleepLocked(ctx context.Context, id string) error {
	row, err := m.cfg.Store.GetSandbox(ctx, id)
	if err != nil {
		return err
	}
	if row.State == store.SandboxSleeping {
		return nil
	}
	if row.State != store.SandboxRunning {
		return store.ErrConflict
	}
	vm, err := m.vmFor(row)
	if err != nil {
		return err
	}
	dir := filepath.Join(runtime.SandboxDir(m.cfg.Root, id), "sleep")
	if err := m.cfg.Runtime.Pause(ctx, vm); err != nil {
		return err
	}
	if _, err := m.cfg.Runtime.SnapshotPaused(ctx, vm, dir); err != nil {
		_ = os.RemoveAll(dir)
		_ = m.cfg.Runtime.Resume(ctx, vm)
		return err
	}
	now := m.now()
	if err := m.cfg.Store.SetSandboxState(ctx, id, store.SandboxSleeping); err != nil {
		return err
	}
	m.event(ctx, id, store.SandboxRunning, store.SandboxSleeping, "sleep", now)
	m.release(context.WithoutCancel(ctx), id, true)
	row.State = store.SandboxSleeping
	m.arm(row)
	return nil
}

// Wake restores a sleeping sandbox and returns it running. A request that
// needs the guest calls it, and the caller waits for the restore.
func (m *Manager) Wake(ctx context.Context, id string) (store.Sandbox, error) {
	unlock := m.lockOps(id)
	defer unlock()
	return m.wakeLocked(ctx, id)
}

// wakeLocked restores a sleeping sandbox from its own image. The caller holds
// the sandbox's lifecycle lock.
func (m *Manager) wakeLocked(ctx context.Context, id string) (store.Sandbox, error) {
	row, err := m.cfg.Store.GetSandbox(ctx, id)
	if err != nil {
		return store.Sandbox{}, err
	}
	if row.State == store.SandboxRunning {
		return row, nil
	}
	if row.State != store.SandboxSleeping {
		return store.Sandbox{}, store.ErrConflict
	}
	tpl, err := m.cfg.Store.GetTemplate(ctx, row.TemplateName)
	if err != nil {
		return store.Sandbox{}, err
	}
	sleepDir := filepath.Join(runtime.SandboxDir(m.cfg.Root, id), "sleep")
	if _, err := os.Stat(filepath.Join(sleepDir, "mem")); err != nil {
		m.failWaking(ctx, id, fmt.Sprintf("no sleep image: %v", err))
		return store.Sandbox{}, fmt.Errorf("sandbox: %s: no sleep image: %w", id, err)
	}
	now := m.now()
	m.markActive(id)
	defer m.clearActive(id)
	if err := m.cfg.Store.SetSandboxState(ctx, id, store.SandboxWaking); err != nil {
		return store.Sandbox{}, err
	}
	m.event(ctx, id, store.SandboxSleeping, store.SandboxWaking, "wake", now)
	// The old jail holds stale sockets and links. The durable state of a
	// sleeping sandbox is its overlay, its sleep image and its directory.
	_ = os.RemoveAll(filepath.Join(runtime.SandboxDir(m.cfg.Root, id), "jail"))
	img := image{dir: sleepDir, template: tpl}
	running, err := m.restore(ctx, img, row, nil, restoreOptions{
		from:         store.SandboxWaking,
		reason:       "woken",
		reuseOverlay: true,
	})
	if err != nil {
		m.failWaking(ctx, id, err.Error())
		return store.Sandbox{}, err
	}
	return running, nil
}

// failWaking records a wake that could not complete and removes the host
// resources it left behind.
func (m *Manager) failWaking(ctx context.Context, id, reason string) {
	saveCtx := context.WithoutCancel(ctx)
	m.cleanup(saveCtx, id)
	if err := m.cfg.Store.SetSandboxState(saveCtx, id, store.SandboxFailed); err != nil {
		log.Printf("sandbox: %s: record failure: %v", id, err)
	}
	m.event(saveCtx, id, store.SandboxWaking, store.SandboxFailed, reason, m.now())
	m.stopTimer(id)
}

// ensureRunningLocked returns the sandbox in the running state, waking a
// sleeping one. The caller holds the sandbox's lifecycle lock.
func (m *Manager) ensureRunningLocked(ctx context.Context, id string) (store.Sandbox, error) {
	row, err := m.cfg.Store.GetSandbox(ctx, id)
	if err != nil {
		return store.Sandbox{}, err
	}
	switch row.State {
	case store.SandboxRunning:
		if _, err := m.vmFor(row); err != nil {
			return store.Sandbox{}, err
		}
		return row, nil
	case store.SandboxSleeping:
		return m.wakeLocked(ctx, id)
	}
	return store.Sandbox{}, store.ErrConflict
}

// acquireRunning returns a running sandbox and holds its shared lifecycle
// lock. A sleeping sandbox wakes under the exclusive lock, so two concurrent
// requests share one restore. The caller releases the lock when its work is
// done.
func (m *Manager) acquireRunning(ctx context.Context, id string) (store.Sandbox, func(), error) {
	for {
		unlock := m.rlockOps(id)
		row, err := m.cfg.Store.GetSandbox(ctx, id)
		if err != nil {
			unlock()
			return store.Sandbox{}, nil, err
		}
		if row.State == store.SandboxRunning {
			if _, err := m.vmFor(row); err != nil {
				unlock()
				return store.Sandbox{}, nil, err
			}
			return row, unlock, nil
		}
		unlock()
		if row.State != store.SandboxSleeping {
			return store.Sandbox{}, nil, store.ErrConflict
		}
		wake := m.lockOps(id)
		_, err = m.wakeLocked(ctx, id)
		wake()
		if err != nil {
			return store.Sandbox{}, nil, err
		}
	}
}

// markActive records that a transition for one sandbox is in flight.
func (m *Manager) markActive(id string) {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	m.active[id] = true
}

// clearActive records that no transition for one sandbox is in flight.
func (m *Manager) clearActive(id string) {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	delete(m.active, id)
}

// Active reports whether a transition for one sandbox is in flight. The
// reconciler uses it to leave a creating, waking or stopping row alone while
// a request still works on it.
func (m *Manager) Active(id string) bool {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	return m.active[id]
}

// Adopt installs handles to a sandbox whose process survived a daemon
// restart. The reconciler has already proved the process and its sockets.
func (m *Manager) Adopt(ctx context.Context, row store.Sandbox, vm *runtime.VM, pages snapshot.PageFaultSource, att *network.Attachment) {
	unlock := m.lockOps(row.ID)
	defer unlock()
	m.mu.Lock()
	if vm != nil {
		m.vms[row.ID] = vm
	}
	if pages != nil {
		m.pages[row.ID] = pages
	}
	if att != nil {
		m.attrs[row.ID] = att
	}
	m.mu.Unlock()
	m.arm(row)
}

// DialGuest opens a TCP connection to one guest port. A sleeping sandbox
// wakes first, so the caller waits for the restore. A preview request counts
// as activity, because an idle timer must not sleep a preview that a person
// is using. The connection outlives the call; the caller closes it.
func (m *Manager) DialGuest(ctx context.Context, id string, port int) (net.Conn, error) {
	row, unlock, err := m.acquireRunning(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	m.mu.Lock()
	att := m.attrs[id]
	m.mu.Unlock()
	if att == nil {
		return nil, store.ErrConflict
	}
	conn, err := att.DialGuest(ctx, port)
	if err != nil {
		return nil, err
	}
	m.touch(ctx, row)
	return conn, nil
}

// Has reports whether the manager already holds handles for one sandbox.
func (m *Manager) Has(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.vms[id] != nil
}

// Forget drops the in-memory handles of one sandbox without touching the
// store. The reconciler calls it after it fails a row, so a later destroy
// never touches a dead microVM.
func (m *Manager) Forget(id string) {
	unlock := m.lockOps(id)
	defer unlock()
	m.mu.Lock()
	delete(m.vms, id)
	delete(m.pages, id)
	delete(m.attrs, id)
	m.mu.Unlock()
	m.stopTimer(id)
}

// watchStart records how the application ended. Nothing restarts it: a crash
// the change caused is what a preview exists to reveal, and a restart would
// hide it. The sandbox keeps running, so the person can open a terminal and
// look.
func (m *Manager) watchStart(id string, vm *runtime.VM, port int) {
	go func() {
		ctx := context.Background()
		res, err := vm.Await(ctx, port)
		if err != nil {
			// The sandbox slept, forked or was destroyed under the wait.
			// None of those is the application ending.
			return
		}
		reason := fmt.Sprintf("the application exited with %d", res.ExitCode)
		if tail := strings.TrimSpace(res.Output); tail != "" {
			reason = reason + ": " + tail
		}
		m.event(ctx, id, store.SandboxRunning, store.SandboxRunning, reason, m.now())
	}()
}
