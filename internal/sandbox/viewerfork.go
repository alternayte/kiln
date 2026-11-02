package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/alternayte/kiln/internal/store"
)

// ViewerForkOptions is what a publish call says about viewer forks.
type ViewerForkOptions struct {
	Cap             int
	AllowSecretFork bool
	Credentials     []CredentialInput
}

// LimitError reports that a viewer has no fork because a cap is full. The
// message names the cap, and the viewer reads it.
type LimitError struct {
	Message string
}

func (e *LimitError) Error() string { return e.Message }

// MetadataViewerOrigin is the metadata key that names the origin of a viewer fork.
const MetadataViewerOrigin = "viewer_fork_origin"

// PublishViewerFork serves one guest port so that each viewer login reaches its
// own fork. The first port takes the one snapshot every fork restores from, and
// later ports of the origin share it.
func (m *Manager) PublishViewerFork(ctx context.Context, id string, port int, opts ViewerForkOptions) (store.Published, error) {
	if opts.Cap < 1 {
		return store.Published{}, invalidf("viewer_cap must be greater than zero")
	}
	if err := m.checkPublish(port, store.VisibilityTeam); err != nil {
		return store.Published{}, err
	}
	unlock := m.lockOps(id)
	defer unlock()
	row, err := m.publishable(ctx, id)
	if err != nil {
		return store.Published{}, err
	}
	origin, err := m.cfg.Store.GetViewerOrigin(ctx, id)
	switch {
	case err == nil:
		if origin.Cap != opts.Cap {
			return store.Published{}, store.ErrConflict
		}
		return m.publishLocked(ctx, row, port, store.VisibilityTeam)
	case !errors.Is(err, store.ErrNotFound):
		return store.Published{}, err
	}
	// A hostname that already serves the origin must not change what it serves.
	hosts, err := m.cfg.Store.ListPublished(ctx, id)
	if err != nil {
		return store.Published{}, err
	}
	if len(hosts) > 0 {
		return store.Published{}, store.ErrConflict
	}
	if row.SecretBearing && !opts.AllowSecretFork {
		return store.Published{}, store.ErrConflict
	}
	if row.EgressObserve {
		return store.Published{}, errObserving
	}
	tpl, err := m.cfg.Store.GetTemplate(ctx, row.TemplateName)
	if err != nil {
		return store.Published{}, err
	}
	sealed, err := m.sealCopyCredentials(opts.Credentials, tpl, row.Egress(tpl))
	if err != nil {
		return store.Published{}, err
	}
	// A client that leaves mid-write must not leave the VM paused.
	ctx = context.WithoutCancel(ctx)
	if row, err = m.ensureRunningLocked(ctx, id); err != nil {
		return store.Published{}, err
	}
	vm, err := m.vmFor(row)
	if err != nil {
		return store.Published{}, err
	}
	snapID, err := newID()
	if err != nil {
		return store.Published{}, err
	}
	dir := filepath.Join(m.cfg.Root, "snapshots", snapID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return store.Published{}, err
	}
	if err := m.cfg.Store.SetTimelineDiffOK(ctx, id, false); err != nil {
		_ = os.RemoveAll(dir)
		return store.Published{}, err
	}
	m.fillPages(id)
	files, err := m.cfg.Runtime.SnapshotWithDisk(ctx, vm, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return store.Published{}, err
	}
	if err := m.cfg.Store.CreateSnapshot(ctx, store.Snapshot{
		ID:              snapID,
		TemplateName:    row.TemplateName,
		ParentID:        row.SnapshotID,
		SizeBytes:       snapshotSize(files),
		CreatedAt:       m.now(),
		SecretBearing:   row.SecretBearing,
		Listed:          false,
		OriginSandboxID: row.ID,
		EgressAllow:     row.EgressAllow,
	}); err != nil {
		_ = os.RemoveAll(dir)
		return store.Published{}, err
	}
	writeParentFile(dir, row.SnapshotID, row.TemplateName)
	if err := m.cfg.Store.CreateViewerOrigin(ctx, store.ViewerOrigin{
		SandboxID: id, SnapshotID: snapID, Cap: opts.Cap, Bindings: sealed, CreatedAt: m.now(),
	}); err != nil {
		m.reapSnapshot(ctx, snapID)
		return store.Published{}, err
	}
	published, err := m.publishLocked(ctx, row, port, store.VisibilityTeam)
	if err != nil {
		m.endViewerForks(ctx, id)
		return store.Published{}, err
	}
	return published, nil
}

// ViewerOrigin returns the viewer fork settings of one sandbox.
func (m *Manager) ViewerOrigin(ctx context.Context, id string) (store.ViewerOrigin, error) {
	return m.cfg.Store.GetViewerOrigin(ctx, id)
}

// ViewerForks returns the live fork of each viewer of one origin.
func (m *Manager) ViewerForks(ctx context.Context, id string) ([]store.ViewerFork, error) {
	if _, err := m.cfg.Store.GetSandbox(ctx, id); err != nil {
		return nil, err
	}
	return m.cfg.Store.ListViewerForks(ctx, id)
}

// LiveViewerFork returns the id of the live fork of one viewer. It starts nothing.
func (m *Manager) LiveViewerFork(ctx context.Context, originID, viewer string) (string, bool, error) {
	forks, err := m.cfg.Store.ListViewerForks(ctx, originID)
	if err != nil {
		return "", false, err
	}
	for _, fork := range forks {
		if fork.ViewerID == viewer {
			return fork.ForkID, true, nil
		}
	}
	return "", false, nil
}

// ViewerFork returns the live fork of one viewer, and restores a new one from
// the origin's snapshot when none is live. The origin may sleep: a restore does
// not read it. The call holds the viewer lock of the origin, so two visits make
// one fork and the cap holds.
func (m *Manager) ViewerFork(ctx context.Context, originID, viewer string) (string, error) {
	// Another viewer may wait on this restore, so a client that leaves does not
	// end it.
	ctx = context.WithoutCancel(ctx)
	unlockViewers := m.lockOps("viewers:" + originID)
	defer unlockViewers()
	if id, live, err := m.LiveViewerFork(ctx, originID, viewer); err != nil || live {
		return id, err
	}
	// The shared side keeps a destroy of the origin out, and an exec still runs.
	unlock := m.rlockOps(originID)
	defer unlock()
	origin, err := m.cfg.Store.GetViewerOrigin(ctx, originID)
	if err != nil {
		return "", err
	}
	forks, err := m.cfg.Store.ListViewerForks(ctx, originID)
	if err != nil {
		return "", err
	}
	if len(forks) >= origin.Cap {
		return "", &LimitError{Message: fmt.Sprintf("this preview holds its limit of %d viewer forks", origin.Cap)}
	}
	if err := m.admitViewerFork(ctx); err != nil {
		return "", err
	}
	row, err := m.cfg.Store.GetSandbox(ctx, originID)
	if err != nil {
		return "", err
	}
	snap, err := m.cfg.Store.GetSnapshot(ctx, origin.SnapshotID)
	if err != nil {
		return "", err
	}
	tpl, err := m.cfg.Store.GetTemplate(ctx, row.TemplateName)
	if err != nil {
		return "", err
	}
	metadata := map[string]any{}
	if row.Metadata != "" {
		if err := json.Unmarshal([]byte(row.Metadata), &metadata); err != nil {
			return "", fmt.Errorf("sandbox: %s: metadata: %w", originID, err)
		}
	}
	metadata[MetadataViewerOrigin] = originID
	if row.Metadata, err = encodeMetadata(metadata); err != nil {
		return "", err
	}
	row.Lifecycle = store.LifecycleEphemeral
	img := image{
		dir:           filepath.Join(m.cfg.Root, "snapshots", origin.SnapshotID),
		template:      tpl,
		snapshotID:    origin.SnapshotID,
		secretBearing: snap.SecretBearing,
		egress:        snap.EgressAllow,
		bindings:      origin.Bindings,
	}
	copies, err := m.makeCopies(ctx, img, row, 1)
	if err != nil {
		return "", err
	}
	if err := m.cfg.Store.SetViewerFork(ctx, originID, viewer, copies[0].ID); err != nil {
		_ = m.Destroy(ctx, copies[0].ID)
		return "", err
	}
	return copies[0].ID, nil
}

// admitViewerFork refuses a fork over the tenant's sandbox cap.
func (m *Manager) admitViewerFork(ctx context.Context) error {
	id, ok := store.TenantFrom(ctx)
	if !ok {
		return nil
	}
	tenant, err := m.cfg.Store.GetTenant(ctx, id)
	if err != nil {
		return err
	}
	if tenant.Caps.MaxSandboxes <= 0 {
		return nil
	}
	usage, err := m.cfg.Store.TenantUsage(ctx, id)
	if err != nil {
		return err
	}
	if usage.Sandboxes >= tenant.Caps.MaxSandboxes {
		return &LimitError{Message: fmt.Sprintf("this tenant holds its limit of %d sandboxes", tenant.Caps.MaxSandboxes)}
	}
	return nil
}

// ResetViewerFork destroys the fork of one viewer. The next visit restores a
// clean one.
func (m *Manager) ResetViewerFork(ctx context.Context, originID, viewer string) error {
	ctx = context.WithoutCancel(ctx)
	unlock := m.lockOps("viewers:" + originID)
	defer unlock()
	id, live, err := m.LiveViewerFork(ctx, originID, viewer)
	if err != nil || !live {
		return err
	}
	return m.Destroy(ctx, id)
}

// endViewerForks destroys every fork of an origin, its settings and its
// snapshot. The caller holds the origin's lifecycle lock, so no new fork starts.
func (m *Manager) endViewerForks(ctx context.Context, originID string) {
	origin, err := m.cfg.Store.GetViewerOrigin(ctx, originID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("sandbox: %s: viewer forks: %v", originID, err)
		}
		return
	}
	forks, err := m.cfg.Store.ListViewerForks(ctx, originID)
	if err != nil {
		log.Printf("sandbox: %s: viewer forks: %v", originID, err)
	}
	for _, fork := range forks {
		if err := m.Destroy(ctx, fork.ForkID); err != nil {
			log.Printf("sandbox: %s: destroy viewer fork %s: %v", originID, fork.ForkID, err)
		}
	}
	if err := m.cfg.Store.DeleteViewerOrigin(ctx, originID); err != nil {
		log.Printf("sandbox: %s: viewer origin: %v", originID, err)
		return
	}
	m.reapSnapshot(ctx, origin.SnapshotID)
}
