// Package sandbox owns the sandbox lifecycle: restore, exec, files, snapshot,
// fork, sleep, wake, destroy.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/alternayte/kiln/internal/guestproto"
	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/snapshot"
	"github.com/alternayte/kiln/internal/store"
	"golang.org/x/sys/unix"
)

// ErrExhausted reports that the host has no room for another sandbox.
var ErrExhausted = errors.New("sandbox: exhausted")

// ErrPartial reports that a fork or a restore failed after it made copies.
// Every copy it made in that call was destroyed.
var ErrPartial = errors.New("sandbox: fork did not complete")

// InvalidError reports a request that cannot be accepted.
type InvalidError struct {
	Message string
}

func (e *InvalidError) Error() string { return "invalid: " + e.Message }

// IsInvalid reports whether err is a rejected request.
func IsInvalid(err error) bool {
	var e *InvalidError
	return errors.As(err, &e)
}

func invalidf(format string, args ...any) error {
	return &InvalidError{Message: fmt.Sprintf(format, args...)}
}

// Config holds the sandbox manager dependencies.
type Config struct {
	Root    string
	Store   store.Store
	Runtime *runtime.Firecracker
	Network *network.Manager
	// Pages returns the page fault source for one restore.
	Pages func() snapshot.PageFaultSource
	// KernelPath is the pinned vmlinux the restored state references.
	KernelPath string
	// Secrets maps secret names to values. Values are never written to disk.
	Secrets map[string]string
	// Zone is the preview zone. Published hostnames sit under it. An empty
	// zone refuses every publish.
	Zone string
	// Now returns the current time. Tests set it.
	Now func() time.Time
}

// Manager creates, forks, sleeps, wakes and destroys sandboxes.
type Manager struct {
	cfg Config

	mu    sync.Mutex
	vms   map[string]*runtime.VM
	pages map[string]snapshot.PageFaultSource
	attrs map[string]*network.Attachment

	// opMu guards the lifecycle lock table. One lock per sandbox serializes
	// its state transitions; exec and file requests take the read side so
	// they run together but never during a sleep or a wake.
	opMu sync.Mutex
	ops  map[string]*opLock

	// timerMu guards the single timer each sandbox has for its idle and ttl
	// deadlines.
	timerMu sync.Mutex
	timers  map[string]*time.Timer

	// activeMu guards active, the sandboxes with a transition in flight. The
	// reconciler must not fail a creating, waking or stopping row that a
	// request is still working on.
	activeMu sync.Mutex
	active   map[string]bool
}

// New returns a sandbox manager.
func New(cfg Config) *Manager {
	return &Manager{
		cfg:    cfg,
		vms:    map[string]*runtime.VM{},
		pages:  map[string]snapshot.PageFaultSource{},
		attrs:  map[string]*network.Attachment{},
		ops:    map[string]*opLock{},
		timers: map[string]*time.Timer{},
		active: map[string]bool{},
	}
}

// CreateRequest is one POST /v1/sandboxes body.
type CreateRequest struct {
	Template    string
	Lifecycle   string
	IdleSeconds int
	TTLSeconds  *int
	Metadata    map[string]any
	Secrets     []string
}

func (r CreateRequest) validate() error {
	if r.Template == "" {
		return invalidf("template is required")
	}
	switch r.Lifecycle {
	case store.LifecycleEphemeral, store.LifecyclePersistent:
	default:
		return invalidf("lifecycle must be %q or %q", store.LifecycleEphemeral, store.LifecyclePersistent)
	}
	if r.IdleSeconds <= 0 {
		return invalidf("idle_seconds must be greater than zero")
	}
	if r.TTLSeconds != nil && *r.TTLSeconds <= 0 {
		return invalidf("ttl_seconds must be greater than zero or null")
	}
	return nil
}

// image is one memory and state pair a restore reads: a template snapshot, a
// listed snapshot, the image a fork wrote for its children, or a sandbox's
// own sleep image.
type image struct {
	dir        string
	template   store.Template
	snapshotID string
	secret     bool
	// fromTemplate marks the template's own snapshot, which comes from a
	// fresh VM. A sleep image and a fork image restore memory that already
	// holds the running application, so only this one starts it.
	fromTemplate bool
}

func (i image) memPath() string   { return filepath.Join(i.dir, "mem") }
func (i image) statePath() string { return filepath.Join(i.dir, "state") }

// restoreOptions carries what differs between a create, a fork and a wake.
type restoreOptions struct {
	// from is the state the transition started in, for the event.
	from string
	// reason is the event reason.
	reason string
	// reuseOverlay keeps the writable drive of a waking sandbox. A wake must
	// never recreate it: the drive holds the sandbox files.
	reuseOverlay bool
}

// Create restores a template snapshot into a running sandbox. It returns when
// the sandbox answers a fresh vsock hello and the resume hooks have run.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (store.Sandbox, error) {
	if err := req.validate(); err != nil {
		return store.Sandbox{}, err
	}
	tpl, err := m.cfg.Store.GetTemplate(ctx, req.Template)
	if err != nil {
		return store.Sandbox{}, err
	}
	if tpl.State != store.TemplateReady {
		return store.Sandbox{}, store.ErrConflict
	}
	env := make(map[string]string, len(req.Secrets))
	for _, name := range req.Secrets {
		value, ok := m.cfg.Secrets[name]
		if !ok {
			return store.Sandbox{}, invalidf("unknown secret %q", name)
		}
		env[name] = value
	}
	if err := checkSpace(m.cfg.Root, int64(tpl.DiskMB)); err != nil {
		return store.Sandbox{}, err
	}

	id, err := newID()
	if err != nil {
		return store.Sandbox{}, err
	}
	metadata := "{}"
	if len(req.Metadata) > 0 {
		b, err := json.Marshal(req.Metadata)
		if err != nil {
			return store.Sandbox{}, invalidf("metadata: %v", err)
		}
		metadata = string(b)
	}
	now := m.now()
	m.markActive(id)
	defer m.clearActive(id)
	row, err := m.cfg.Store.CreateSandbox(ctx, store.Sandbox{
		ID:            id,
		TemplateName:  tpl.Name,
		Lifecycle:     req.Lifecycle,
		State:         store.SandboxCreating,
		TTLSeconds:    req.TTLSeconds,
		IdleSeconds:   req.IdleSeconds,
		LastActiveAt:  now,
		Metadata:      metadata,
		CreatedAt:     now,
		SecretBearing: len(env) > 0,
	})
	if err != nil {
		if errors.Is(err, store.ErrExhausted) {
			return store.Sandbox{}, ErrExhausted
		}
		return store.Sandbox{}, err
	}
	m.event(ctx, id, "", store.SandboxCreating, "create", now)

	img := image{
		dir:          m.templateDir(tpl),
		template:     tpl,
		fromTemplate: true,
	}
	running, err := m.restore(ctx, img, row, env, restoreOptions{from: store.SandboxCreating, reason: "restored"})
	if err != nil {
		// The restore must not leave a VM, a TAP or files behind.
		m.cleanup(context.WithoutCancel(ctx), id)
		saveCtx := context.WithoutCancel(ctx)
		if serr := m.cfg.Store.SetSandboxState(saveCtx, id, store.SandboxFailed); serr != nil {
			log.Printf("sandbox: %s: record failure: %v", id, serr)
		}
		m.event(saveCtx, id, store.SandboxCreating, store.SandboxFailed, err.Error(), m.now())
		return store.Sandbox{}, err
	}
	return running, nil
}

// restore brings one sandbox row up from an image. It returns when the guest
// answers a fresh vsock hello. The caller owns the row and every failure.
func (m *Manager) restore(ctx context.Context, img image, row store.Sandbox, env map[string]string, opts restoreOptions) (store.Sandbox, error) {
	if row.VsockCID == nil {
		return store.Sandbox{}, fmt.Errorf("sandbox: %s has no vsock cid", row.ID)
	}
	dir := runtime.SandboxDir(m.cfg.Root, row.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return store.Sandbox{}, err
	}
	overlay := filepath.Join(dir, "overlay.ext4")
	if !opts.reuseOverlay {
		if err := createOverlay(ctx, overlay, int64(img.template.DiskMB)); err != nil {
			return store.Sandbox{}, err
		}
	}
	att, err := m.cfg.Network.Attach(ctx, row.ID, img.template.EgressAllow)
	if err != nil {
		return store.Sandbox{}, err
	}
	m.mu.Lock()
	m.attrs[row.ID] = att
	m.mu.Unlock()
	if err := m.cfg.Store.SetSandboxRuntime(ctx, row.ID, att.TAPName, row.VsockCID, 0); err != nil {
		return store.Sandbox{}, err
	}

	pages := m.cfg.Pages()
	sock := runtime.RestoreUffdSocket(m.cfg.Root, row.ID)
	if err := pages.Start(ctx, img.memPath(), sock); err != nil {
		return store.Sandbox{}, err
	}
	m.mu.Lock()
	m.pages[row.ID] = pages
	m.mu.Unlock()

	vm, err := m.cfg.Runtime.Restore(ctx, runtime.RestoreSpec{
		Spec: runtime.Spec{
			ID:          row.ID,
			KernelPath:  m.cfg.KernelPath,
			RootfsPath:  filepath.Join(m.templateDir(img.template), "rootfs.ext4"),
			OverlayPath: overlay,
			VCPUs:       img.template.VCPUs,
			MemoryMiB:   img.template.MemoryMB,
			VsockCID:    uint32(*row.VsockCID),
			VsockPort:   guestproto.Port,
			TAPName:     att.TAPName,
		},
		StatePath:  img.statePath(),
		UffdSocket: sock,
	})
	if err != nil {
		return store.Sandbox{}, err
	}
	m.mu.Lock()
	m.vms[row.ID] = vm
	m.mu.Unlock()

	entropy := make([]byte, 64)
	if _, err := rand.Read(entropy); err != nil {
		return store.Sandbox{}, err
	}
	if err := vm.ResumeHooks(ctx, entropy, time.Now().UnixNano(), row.ID, env); err != nil {
		return store.Sandbox{}, err
	}
	if len(img.template.Start) > 0 && img.template.StartPort > 0 {
		// The application starts after the resume hooks, which place the
		// hostname and the secrets it reads. Only a template snapshot needs
		// starting: a sleep image and a fork image restore memory that
		// already holds the process.
		if img.fromTemplate {
			if err := vm.Start(ctx, img.template.Start, img.template.StartPort,
				guestproto.StartDeadlineSeconds, env); err != nil {
				return store.Sandbox{}, fmt.Errorf("start %v on port %d: %w",
					img.template.Start, img.template.StartPort, err)
			}
		}
		// The watch dies with the VM it watches, so every restore takes it
		// up again. Without this a sandbox that slept once reports no crash
		// for the rest of its life.
		m.watchStart(row.ID, vm, img.template.StartPort)
	}
	if err := m.cfg.Store.SetSandboxRuntime(ctx, row.ID, att.TAPName, row.VsockCID, vm.PID); err != nil {
		return store.Sandbox{}, err
	}
	now := m.now()
	row.State = store.SandboxRunning
	row.TapName = att.TAPName
	row.PID = vm.PID
	row.LastActiveAt = now
	if err := m.cfg.Store.TouchSandbox(ctx, row.ID, now); err != nil {
		return store.Sandbox{}, err
	}
	if err := m.cfg.Store.SetSandboxState(ctx, row.ID, store.SandboxRunning); err != nil {
		return store.Sandbox{}, err
	}
	m.event(ctx, row.ID, opts.from, store.SandboxRunning, opts.reason, now)
	m.arm(row)
	return row, nil
}

// Snapshot writes the sandbox's memory into a listed snapshot. With stop set
// the sandbox also sleeps: its own image is written under its directory and
// the VM stops. The sandbox keeps its id and its files.
func (m *Manager) Snapshot(ctx context.Context, id string, stop bool) (store.Snapshot, error) {
	unlock := m.lockOps(id)
	defer unlock()
	row, err := m.ensureRunningLocked(ctx, id)
	if err != nil {
		return store.Snapshot{}, err
	}
	vm, err := m.vmFor(row)
	if err != nil {
		return store.Snapshot{}, err
	}
	snapID, err := newID()
	if err != nil {
		return store.Snapshot{}, err
	}
	dir := filepath.Join(m.cfg.Root, "snapshots", snapID)
	var files runtime.SnapshotFiles
	if stop {
		if err := m.cfg.Runtime.Pause(ctx, vm); err != nil {
			return store.Snapshot{}, err
		}
		if files, err = m.cfg.Runtime.SnapshotPaused(ctx, vm, dir); err != nil {
			_ = m.cfg.Runtime.Resume(ctx, vm)
			return store.Snapshot{}, err
		}
		sleepDir := filepath.Join(runtime.SandboxDir(m.cfg.Root, id), "sleep")
		if _, err := m.cfg.Runtime.SnapshotPaused(ctx, vm, sleepDir); err != nil {
			_ = os.RemoveAll(sleepDir)
			_ = m.cfg.Runtime.Resume(ctx, vm)
			return store.Snapshot{}, err
		}
	} else if files, err = m.cfg.Runtime.Snapshot(ctx, vm, dir); err != nil {
		return store.Snapshot{}, err
	}
	now := m.now()
	snap := store.Snapshot{
		ID:              snapID,
		TemplateName:    row.TemplateName,
		ParentID:        row.SnapshotID,
		SizeBytes:       snapshotSize(files),
		CreatedAt:       now,
		SecretBearing:   row.SecretBearing,
		Listed:          true,
		OriginSandboxID: row.ID,
	}
	if err := m.cfg.Store.CreateSnapshot(ctx, snap); err != nil {
		_ = os.RemoveAll(dir)
		if stop {
			_ = os.RemoveAll(filepath.Join(runtime.SandboxDir(m.cfg.Root, id), "sleep"))
			_ = m.cfg.Runtime.Resume(ctx, vm)
		}
		return store.Snapshot{}, err
	}
	writeParentFile(dir, row.SnapshotID, row.TemplateName)
	if stop {
		if err := m.cfg.Store.SetSandboxState(ctx, id, store.SandboxSleeping); err != nil {
			return store.Snapshot{}, err
		}
		m.event(ctx, id, store.SandboxRunning, store.SandboxSleeping, "sleep", now)
		m.release(context.WithoutCancel(ctx), id, true)
		row.State = store.SandboxSleeping
		m.arm(row)
	} else {
		m.touch(ctx, row)
	}
	return snap, nil
}

// Fork snapshots a running sandbox and restores count copies from that one
// image. Every copy gets a fresh overlay and its own page-fault source. A
// failure at any copy destroys every copy made in the call. A sleeping source
// wakes first.
func (m *Manager) Fork(ctx context.Context, id string, count int, allowSecret bool) ([]store.Sandbox, error) {
	if count < 1 {
		return nil, invalidf("count must be greater than zero")
	}
	unlock := m.lockOps(id)
	defer unlock()
	row, err := m.ensureRunningLocked(ctx, id)
	if err != nil {
		return nil, err
	}
	vm, err := m.vmFor(row)
	if err != nil {
		return nil, err
	}
	if row.SecretBearing && !allowSecret {
		return nil, store.ErrConflict
	}
	tpl, err := m.cfg.Store.GetTemplate(ctx, row.TemplateName)
	if err != nil {
		return nil, err
	}
	snapID, err := newID()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(m.cfg.Root, "snapshots", snapID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	files, err := m.cfg.Runtime.Snapshot(ctx, vm, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	img := image{dir: dir, template: tpl, snapshotID: snapID, secret: row.SecretBearing}
	if err := m.cfg.Store.CreateSnapshot(ctx, store.Snapshot{
		ID:              snapID,
		TemplateName:    row.TemplateName,
		ParentID:        row.SnapshotID,
		SizeBytes:       snapshotSize(files),
		CreatedAt:       m.now(),
		SecretBearing:   row.SecretBearing,
		Listed:          false,
		OriginSandboxID: row.ID,
	}); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	writeParentFile(dir, row.SnapshotID, row.TemplateName)
	m.touch(ctx, row)

	copies, err := m.makeCopies(ctx, img, row, count)
	if err != nil {
		m.reapSnapshot(context.WithoutCancel(ctx), snapID)
		return nil, err
	}
	return copies, nil
}

// RestoreSnapshot copies a listed snapshot into count new sandboxes. The
// copies inherit the lifecycle and metadata of the sandbox the snapshot came
// from.
func (m *Manager) RestoreSnapshot(ctx context.Context, snapshotID string, count int, allowSecret bool) ([]store.Sandbox, error) {
	if count < 1 {
		return nil, invalidf("count must be greater than zero")
	}
	snap, err := m.cfg.Store.GetSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	if !snap.Listed {
		return nil, store.ErrNotFound
	}
	if snap.SecretBearing && !allowSecret {
		return nil, store.ErrConflict
	}
	origin, err := m.cfg.Store.GetSandbox(ctx, snap.OriginSandboxID)
	if err != nil {
		// The snapshot outlived the sandbox it came from, so the lifecycle
		// fields it was created with are gone.
		return nil, store.ErrConflict
	}
	tpl, err := m.cfg.Store.GetTemplate(ctx, snap.TemplateName)
	if err != nil {
		return nil, err
	}
	img := image{
		dir:        filepath.Join(m.cfg.Root, "snapshots", snapshotID),
		template:   tpl,
		snapshotID: snapshotID,
		secret:     snap.SecretBearing,
	}
	m.touch(ctx, origin)
	copies, err := m.makeCopies(ctx, img, origin, count)
	if err != nil {
		return nil, err
	}
	return copies, nil
}

// Snapshots returns every listed snapshot row.
func (m *Manager) Snapshots(ctx context.Context) ([]store.Snapshot, error) {
	return m.cfg.Store.ListSnapshots(ctx)
}

// SnapshotInfo returns one listed snapshot row. An unlisted image is not
// visible and reports not found.
func (m *Manager) SnapshotInfo(ctx context.Context, id string) (store.Snapshot, error) {
	snap, err := m.cfg.Store.GetSnapshot(ctx, id)
	if err != nil {
		return store.Snapshot{}, err
	}
	if !snap.Listed {
		return store.Snapshot{}, store.ErrNotFound
	}
	return snap, nil
}

// DeleteSnapshot removes a listed snapshot once no live sandbox reads it.
func (m *Manager) DeleteSnapshot(ctx context.Context, id string) error {
	snap, err := m.SnapshotInfo(ctx, id)
	if err != nil {
		return err
	}
	live, err := m.cfg.Store.CountRestoredChildren(ctx, snap.ID)
	if err != nil {
		return err
	}
	if live > 0 {
		return store.ErrConflict
	}
	// Delete the row first. A failed row delete then leaves the files, not a
	// row that names missing files.
	if err := m.cfg.Store.DeleteSnapshot(ctx, snap.ID); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(m.cfg.Root, "snapshots", snap.ID))
}

// makeCopies creates count rows from one image and restores each of them. Any
// failure destroys every copy this call made and reports ErrPartial, so the
// caller can never be left with a partial fork.
func (m *Manager) makeCopies(ctx context.Context, img image, base store.Sandbox, count int) ([]store.Sandbox, error) {
	made := make([]store.Sandbox, 0, count)
	fail := func(err error) ([]store.Sandbox, error) {
		for _, sb := range made {
			_ = m.Destroy(context.WithoutCancel(ctx), sb.ID)
		}
		return nil, fmt.Errorf("%w: %v", ErrPartial, err)
	}
	for i := 0; i < count; i++ {
		id, err := newID()
		if err != nil {
			return fail(err)
		}
		now := m.now()
		m.markActive(id)
		row, err := m.cfg.Store.CreateSandbox(ctx, store.Sandbox{
			ID:            id,
			TemplateName:  base.TemplateName,
			SnapshotID:    img.snapshotID,
			Lifecycle:     base.Lifecycle,
			State:         store.SandboxCreating,
			TTLSeconds:    copyInt(base.TTLSeconds),
			IdleSeconds:   base.IdleSeconds,
			LastActiveAt:  now,
			Metadata:      base.Metadata,
			CreatedAt:     now,
			SecretBearing: img.secret,
		})
		if err != nil {
			m.clearActive(id)
			if errors.Is(err, store.ErrExhausted) {
				return fail(ErrExhausted)
			}
			return fail(err)
		}
		m.event(ctx, id, "", store.SandboxCreating, "fork", now)
		made = append(made, row)
		stored, err := m.restore(ctx, img, row, nil, restoreOptions{from: store.SandboxCreating, reason: "restored"})
		m.clearActive(id)
		if err != nil {
			return fail(err)
		}
		made[len(made)-1] = stored
	}
	return made, nil
}

// Destroy stops a sandbox and removes every host resource named after it. It
// is idempotent and works on a sleeping sandbox without waking it.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	unlock := m.lockOps(id)
	defer unlock()
	return m.destroyLocked(ctx, id)
}

func (m *Manager) destroyLocked(ctx context.Context, id string) error {
	row, err := m.cfg.Store.GetSandbox(ctx, id)
	if err != nil {
		return err
	}
	if row.State == store.SandboxDestroyed || row.DestroyedAt != nil {
		m.stopTimer(id)
		return nil
	}
	m.markActive(id)
	defer m.clearActive(id)
	if err := m.cfg.Store.SetSandboxState(ctx, id, store.SandboxStopping); err != nil {
		return err
	}
	m.event(ctx, id, row.State, store.SandboxStopping, "destroy", m.now())
	// Publishing dies with the sandbox. The hostnames 404 from then on.
	if err := m.cfg.Store.DeletePublishedForSandbox(ctx, id); err != nil {
		return err
	}
	m.cleanup(ctx, id)
	if err := os.RemoveAll(runtime.SandboxDir(m.cfg.Root, id)); err != nil {
		return err
	}
	now := m.now()
	if err := m.cfg.Store.DestroySandbox(ctx, id, now); err != nil {
		return err
	}
	m.event(ctx, id, store.SandboxStopping, store.SandboxDestroyed, "destroyed", now)
	m.stopTimer(id)
	m.reapSnapshot(ctx, row.SnapshotID)
	return nil
}

// Get returns one sandbox row.
func (m *Manager) Get(ctx context.Context, id string) (store.Sandbox, error) {
	return m.cfg.Store.GetSandbox(ctx, id)
}

// List returns every sandbox row.
func (m *Manager) List(ctx context.Context) ([]store.Sandbox, error) {
	return m.cfg.Store.ListSandboxes(ctx)
}

// cleanup stops every host resource of one sandbox. It is best effort: each
// step runs even when an earlier one fails.
func (m *Manager) cleanup(ctx context.Context, id string) {
	m.release(ctx, id, false)
}

// release stops one sandbox's host resources. With paused set the VM is
// killed without resuming it, which is how a sleep ends.
func (m *Manager) release(ctx context.Context, id string, paused bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	m.mu.Lock()
	vm := m.vms[id]
	pages := m.pages[id]
	att := m.attrs[id]
	delete(m.vms, id)
	delete(m.pages, id)
	delete(m.attrs, id)
	m.mu.Unlock()

	if vm != nil {
		var err error
		if paused {
			err = m.cfg.Runtime.StopPaused(ctx, vm)
		} else {
			err = m.cfg.Runtime.Stop(ctx, vm)
		}
		if err != nil {
			log.Printf("sandbox: %s: stop: %v", id, err)
		}
	}
	if pages != nil {
		if err := pages.Stop(); err != nil {
			log.Printf("sandbox: %s: snapfault: %v", id, err)
		}
	}
	if att != nil {
		if err := att.Detach(ctx); err != nil {
			log.Printf("sandbox: %s: network: %v", id, err)
		}
	}
}

// reapSnapshot removes an unlisted image once no live sandbox reads it.
func (m *Manager) reapSnapshot(ctx context.Context, snapshotID string) {
	if snapshotID == "" {
		return
	}
	snap, err := m.cfg.Store.GetSnapshot(ctx, snapshotID)
	if err != nil || snap.Listed {
		return
	}
	live, err := m.cfg.Store.CountRestoredChildren(ctx, snapshotID)
	if err != nil || live != 0 {
		return
	}
	if err := m.cfg.Store.DeleteSnapshot(ctx, snapshotID); err != nil {
		return
	}
	if err := os.RemoveAll(filepath.Join(m.cfg.Root, "snapshots", snapshotID)); err != nil {
		log.Printf("sandbox: snapshot %s: remove: %v", snapshotID, err)
	}
}

// running returns the live VM of a running sandbox.
func (m *Manager) running(ctx context.Context, id string) (*runtime.VM, error) {
	row, err := m.cfg.Store.GetSandbox(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.State != store.SandboxRunning {
		return nil, store.ErrConflict
	}
	return m.vmFor(row)
}

// vmFor returns the in-memory VM handle of one sandbox row.
func (m *Manager) vmFor(row store.Sandbox) (*runtime.VM, error) {
	m.mu.Lock()
	vm := m.vms[row.ID]
	m.mu.Unlock()
	if vm == nil {
		return nil, store.ErrConflict
	}
	return vm, nil
}

func (m *Manager) event(ctx context.Context, id, from, to, reason string, at time.Time) {
	if err := m.cfg.Store.AppendEvent(ctx, store.Event{
		SandboxID: id,
		FromState: from,
		ToState:   to,
		Reason:    reason,
		At:        at,
	}); err != nil {
		log.Printf("sandbox: %s: event: %v", id, err)
	}
}

func (m *Manager) now() time.Time {
	if m.cfg.Now != nil {
		return m.cfg.Now()
	}
	return time.Now().UTC()
}

// newID returns a short random sandbox id. It is safe as a directory name and
// as part of a TAP device name.
func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// snapshotSize is the size of one memory and state pair on disk.
func snapshotSize(files runtime.SnapshotFiles) int64 {
	var total int64
	for _, path := range []string{files.MemPath, files.StatePath} {
		if fi, err := os.Stat(path); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// writeParentFile records what an image descends from. The on-disk layout
// keeps it next to the memory and state.
func writeParentFile(dir, parentID, templateName string) {
	parent := parentID
	if parent == "" {
		parent = templateName
	}
	if err := os.WriteFile(filepath.Join(dir, "parent"), []byte(parent+"\n"), 0o644); err != nil {
		log.Printf("sandbox: %s: parent file: %v", dir, err)
	}
}

// createOverlay makes the sparse writable drive of one sandbox.
func createOverlay(ctx context.Context, path string, sizeMB int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := f.Truncate(sizeMB << 20); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "mke2fs", "-q", "-F", "-t", "ext4", "-L", "kiln-overlay", path).CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox: mke2fs: %w: %s", err, out)
	}
	return nil
}

// checkSpace refuses a create that cannot fit the writable drive.
func checkSpace(root string, needMB int64) error {
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return err
	}
	if int64(st.Bavail)*int64(st.Bsize) < needMB<<20 {
		return ErrExhausted
	}
	return nil
}

// copyInt copies an optional number, so a copy never shares a pointer with
// the row it came from.
func copyInt(v *int) *int {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}

// Exec runs one command in a running sandbox. A sleeping sandbox wakes first.
func (m *Manager) Exec(ctx context.Context, id string, req runtime.ExecRequest, onOutput func(byte, []byte) error) (runtime.ExecResult, error) {
	row, unlock, err := m.acquireRunning(ctx, id)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	defer unlock()
	vm, err := m.vmFor(row)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	res, err := vm.ExecStream(ctx, req, onOutput)
	if err == nil {
		m.touch(ctx, row)
	}
	return res, err
}

// Terminal opens an interactive terminal in a running sandbox. A
// sleeping sandbox wakes first. The read lock is held until the session is
// closed, so the sandbox cannot sleep under an open shell.
func (m *Manager) Terminal(ctx context.Context, id string, size guestproto.Winsize) (*lockedTerminal, error) {
	row, unlock, err := m.acquireRunning(ctx, id)
	if err != nil {
		return nil, err
	}
	vm, err := m.vmFor(row)
	if err != nil {
		unlock()
		return nil, err
	}
	term, err := vm.Terminal(ctx, size, "/", nil)
	if err != nil {
		unlock()
		return nil, err
	}
	m.touch(ctx, row)
	return &lockedTerminal{Terminal: term, unlock: unlock}, nil
}

// OpenFile opens one guest file for reading. A sleeping sandbox wakes first.
// The read lock is held until the reader is closed, so the sandbox cannot
// sleep mid-read.
func (m *Manager) OpenFile(ctx context.Context, id, path string) (io.ReadCloser, error) {
	row, unlock, err := m.acquireRunning(ctx, id)
	if err != nil {
		return nil, err
	}
	vm, err := m.vmFor(row)
	if err != nil {
		unlock()
		return nil, err
	}
	r, err := vm.OpenFile(ctx, path)
	if err != nil {
		unlock()
		return nil, err
	}
	m.touch(ctx, row)
	return &lockedReader{ReadCloser: r, unlock: unlock}, nil
}

// WriteFile writes one guest file. A sleeping sandbox wakes first.
func (m *Manager) WriteFile(ctx context.Context, id, path string, body io.Reader) error {
	row, unlock, err := m.acquireRunning(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	vm, err := m.vmFor(row)
	if err != nil {
		return err
	}
	if err := vm.WriteFile(ctx, path, body); err != nil {
		return err
	}
	m.touch(ctx, row)
	return nil
}

// templateDir is the directory of one template's files. The tenant names the
// parent, so one template name serves every tenant without collision.
func (m *Manager) templateDir(tpl store.Template) string {
	tenant := tpl.TenantID
	if tenant == "" {
		tenant = store.DefaultTenant
	}
	return filepath.Join(m.cfg.Root, "templates", tenant, tpl.Name)
}
