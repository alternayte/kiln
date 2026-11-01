package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/store"
)

// maxPoints is the longest chain of one sandbox. A chain must not slow every
// restore.
const maxPoints = 32

// maxPointLabel bounds the label of one point.
const maxPointLabel = 200

// pointDir is where the files of one point live.
func (m *Manager) pointDir(id, point string) string {
	return filepath.Join(runtime.SandboxDir(m.cfg.Root, id), "timeline", point)
}

// allocated counts the bytes the files of one point hold on disk. A diff and a
// drive copy are sparse, so their length says nothing.
func allocated(dir string) int64 {
	var total int64
	for _, name := range []string{"mem", "state", runtime.OverlayFile} {
		var st syscall.Stat_t
		if err := syscall.Stat(filepath.Join(dir, name), &st); err == nil {
			total += st.Blocks * 512
		}
	}
	return total
}

// MarkPoint writes a timeline point of a running sandbox. A sleeping sandbox
// wakes first.
func (m *Manager) MarkPoint(ctx context.Context, id, label string) (store.TimelinePoint, error) {
	if len(label) > maxPointLabel {
		return store.TimelinePoint{}, invalidf("label holds %d bytes, more than %d", len(label), maxPointLabel)
	}
	unlock := m.lockOps(id)
	defer unlock()
	// A client that leaves mid-write must not leave the VM paused.
	ctx = context.WithoutCancel(ctx)
	row, err := m.ensureRunningLocked(ctx, id)
	if err != nil {
		return store.TimelinePoint{}, err
	}
	if row.EgressObserve {
		return store.TimelinePoint{}, errObserving
	}
	tpl, err := m.cfg.Store.GetTemplate(ctx, row.TemplateName)
	if err != nil {
		return store.TimelinePoint{}, err
	}
	if !tpl.Timeline {
		return store.TimelinePoint{}, fmt.Errorf("%w: template %q has no timeline: true", store.ErrConflict, tpl.Name)
	}
	vm, err := m.vmFor(row)
	if err != nil {
		return store.TimelinePoint{}, err
	}
	points, err := m.cfg.Store.ListTimelinePoints(ctx, id)
	if err != nil {
		return store.TimelinePoint{}, err
	}
	diffOK, err := m.cfg.Store.TimelineDiffOK(ctx, id)
	if err != nil {
		return store.TimelinePoint{}, err
	}
	if err := checkSpace(m.cfg.Root, int64(tpl.MemoryMB)); err != nil {
		return store.TimelinePoint{}, err
	}
	pointID, err := newID()
	if err != nil {
		return store.TimelinePoint{}, err
	}
	dir := m.pointDir(id, pointID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return store.TimelinePoint{}, err
	}
	// A snapshot resets the dirty pages even when the row never lands, so the
	// flag comes down first and goes up only after the row is in.
	if err := m.cfg.Store.SetTimelineDiffOK(ctx, id, false); err != nil {
		_ = os.RemoveAll(dir)
		return store.TimelinePoint{}, err
	}
	kind := store.PointFull
	began := time.Now()
	if diffOK && len(points) > 0 {
		kind = store.PointDiff
		_, err = m.cfg.Runtime.SnapshotDiff(ctx, vm, dir)
	} else {
		m.fillPages(id)
		_, err = m.cfg.Runtime.SnapshotWithDisk(ctx, vm, dir)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return store.TimelinePoint{}, err
	}
	log.Printf("sandbox: %s: point %s: %s write took %s", id, pointID, kind, time.Since(began))
	point, err := m.cfg.Store.CreateTimelinePoint(ctx, store.TimelinePoint{
		ID: pointID, SandboxID: id, Label: label, Kind: kind, SizeBytes: allocated(dir),
		SecretBearing: row.SecretBearing, EgressAllow: row.EgressAllow, CreatedAt: m.now(),
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return store.TimelinePoint{}, err
	}
	if err := m.cfg.Store.SetTimelineDiffOK(ctx, id, true); err != nil {
		return store.TimelinePoint{}, err
	}
	m.touch(ctx, row)
	if err := m.enforcePointLimits(ctx, id, tpl); err != nil {
		return store.TimelinePoint{}, err
	}
	// A merge can have rewritten what the new point holds in bytes.
	points, err = m.cfg.Store.ListTimelinePoints(ctx, id)
	if err != nil {
		return store.TimelinePoint{}, err
	}
	for _, p := range points {
		if p.ID == point.ID {
			return p, nil
		}
	}
	return point, nil
}

// Points lists the chain of one sandbox.
func (m *Manager) Points(ctx context.Context, id string) ([]store.TimelinePoint, error) {
	if _, err := m.cfg.Store.GetSandbox(ctx, id); err != nil {
		return nil, err
	}
	return m.cfg.Store.ListTimelinePoints(ctx, id)
}

// DeletePoint removes one point, and its pages move into the next point.
func (m *Manager) DeletePoint(ctx context.Context, id, pointID string) error {
	unlock := m.lockOps(id)
	defer unlock()
	ctx = context.WithoutCancel(ctx)
	points, at, err := m.findPoint(ctx, id, pointID)
	if err != nil {
		return err
	}
	if at < len(points)-1 {
		return m.mergeForward(ctx, points[at], points[at+1], false)
	}
	// The live VM counts dirty pages from this point, so a diff of it has no
	// base once it goes.
	if err := m.cfg.Store.SetTimelineDiffOK(ctx, id, false); err != nil {
		return err
	}
	if err := m.cfg.Store.DeleteTimelinePoints(ctx, []string{pointID}); err != nil {
		return err
	}
	return os.RemoveAll(m.pointDir(id, pointID))
}

// findPoint returns the chain of a sandbox that has a timeline to act on, and
// the place of one point in it.
func (m *Manager) findPoint(ctx context.Context, id, pointID string) ([]store.TimelinePoint, int, error) {
	row, err := m.cfg.Store.GetSandbox(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	if row.State != store.SandboxRunning && row.State != store.SandboxSleeping {
		return nil, 0, store.ErrConflict
	}
	points, err := m.cfg.Store.ListTimelinePoints(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	for i, p := range points {
		if p.ID == pointID {
			return points, i, nil
		}
	}
	return nil, 0, fmt.Errorf("%w: sandbox %s has no point %s", store.ErrNotFound, id, pointID)
}

// mergeForward moves the pages of one point into the next one and removes it.
// The merge writes a new file and renames it into place. A VM or a fork reads
// the old inode, and a crash at any step leaves a chain that gives the same
// memory, because the new file only repeats pages the chain already applies.
func (m *Manager) mergeForward(ctx context.Context, older, next store.TimelinePoint, count bool) error {
	id := older.SandboxID
	olderDir, nextDir := m.pointDir(id, older.ID), m.pointDir(id, next.ID)
	if next.Kind == store.PointDiff {
		tmp := filepath.Join(nextDir, "mem.merging")
		if err := runtime.CopySparse(ctx, filepath.Join(olderDir, "mem"), tmp); err != nil {
			return err
		}
		if err := m.cfg.Runtime.RebaseMemory(ctx, tmp, filepath.Join(nextDir, "mem")); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, filepath.Join(nextDir, "mem")); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		next.Kind = older.Kind
	}
	next.SizeBytes = allocated(nextDir)
	if count {
		next.Merged += older.Merged + 1
	}
	if err := m.cfg.Store.ReplaceTimelinePoint(ctx, older.ID, next); err != nil {
		return err
	}
	return os.RemoveAll(olderDir)
}

// enforcePointLimits merges the two oldest points until the chain fits. The
// newest point always stays.
func (m *Manager) enforcePointLimits(ctx context.Context, id string, tpl store.Template) error {
	limit := (2*int64(tpl.MemoryMB) + int64(tpl.DiskMB)) << 20
	for {
		points, err := m.cfg.Store.ListTimelinePoints(ctx, id)
		if err != nil {
			return err
		}
		var total int64
		for _, p := range points {
			total += p.SizeBytes
		}
		if len(points) < 2 || (len(points) <= maxPoints && total <= limit) {
			return nil
		}
		if err := m.mergeForward(ctx, points[0], points[1], true); err != nil {
			return err
		}
	}
}

// fullPoint makes one point hold all of memory and keeps that result, so a
// second use costs nothing. A later diff still applies on top of it.
func (m *Manager) fullPoint(ctx context.Context, points []store.TimelinePoint, at int) error {
	target := points[at]
	if target.Kind == store.PointFull {
		return nil
	}
	base := at
	for base > 0 && points[base].Kind != store.PointFull {
		base--
	}
	if points[base].Kind != store.PointFull {
		return fmt.Errorf("sandbox: %s: the chain of point %s holds no full point", target.SandboxID, target.ID)
	}
	id, dir := target.SandboxID, m.pointDir(target.SandboxID, target.ID)
	tmp := filepath.Join(dir, "mem.merging")
	if err := runtime.CopySparse(ctx, filepath.Join(m.pointDir(id, points[base].ID), "mem"), tmp); err != nil {
		return err
	}
	for _, p := range points[base+1 : at+1] {
		if err := m.cfg.Runtime.RebaseMemory(ctx, tmp, filepath.Join(m.pointDir(id, p.ID), "mem")); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, filepath.Join(dir, "mem")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	target.Kind = store.PointFull
	target.SizeBytes = allocated(dir)
	points[at] = target
	return m.cfg.Store.UpdateTimelinePoint(ctx, target)
}

// Rewind stops the guest and restores one point under the same sandbox id and
// hostnames. Every later point goes. The row keeps its current egress policy and
// bindings, because a rewind must not bring back a wider policy or a revoked key.
func (m *Manager) Rewind(ctx context.Context, id, pointID string) (store.Sandbox, error) {
	unlock := m.lockOps(id)
	defer unlock()
	ctx = context.WithoutCancel(ctx)
	points, at, err := m.findPoint(ctx, id, pointID)
	if err != nil {
		return store.Sandbox{}, err
	}
	row, err := m.cfg.Store.GetSandbox(ctx, id)
	if err != nil {
		return store.Sandbox{}, err
	}
	tpl, err := m.cfg.Store.GetTemplate(ctx, row.TemplateName)
	if err != nil {
		return store.Sandbox{}, err
	}
	if err := m.fullPoint(ctx, points, at); err != nil {
		return store.Sandbox{}, err
	}
	if err := m.cfg.Store.SetTimelineDiffOK(ctx, id, false); err != nil {
		return store.Sandbox{}, err
	}
	m.markActive(id)
	defer m.clearActive(id)
	if err := m.cfg.Store.SetSandboxState(ctx, id, store.SandboxWaking); err != nil {
		return store.Sandbox{}, err
	}
	m.event(ctx, id, row.State, store.SandboxWaking, "rewind "+pointID, m.now())
	if row.State == store.SandboxRunning {
		// The VM stops as a sleep ends it: a plain stop removes the sandbox
		// directory, and that holds the timeline.
		if vm, err := m.vmFor(row); err == nil {
			_ = m.cfg.Runtime.Pause(ctx, vm)
		}
		m.release(ctx, id, true)
	}
	sandboxDir := runtime.SandboxDir(m.cfg.Root, id)
	_ = os.RemoveAll(filepath.Join(sandboxDir, "jail"))
	dir := m.pointDir(id, pointID)
	overlay := filepath.Join(sandboxDir, runtime.OverlayFile)
	_ = os.Remove(overlay)
	if err := runtime.CopySparse(ctx, filepath.Join(dir, runtime.OverlayFile), overlay); err != nil {
		m.failWaking(ctx, id, err.Error())
		return store.Sandbox{}, err
	}
	running, err := m.restore(ctx, image{dir: dir, template: tpl}, row, nil, restoreOptions{
		from:         store.SandboxWaking,
		reason:       "rewound",
		reuseOverlay: true,
	})
	if err != nil {
		m.failWaking(ctx, id, err.Error())
		return store.Sandbox{}, err
	}
	// An old sleep image must not stay as a way back to the state it held.
	_ = os.RemoveAll(filepath.Join(sandboxDir, "sleep"))
	later := make([]string, 0, len(points)-at-1)
	for _, p := range points[at+1:] {
		later = append(later, p.ID)
	}
	if err := m.cfg.Store.DeleteTimelinePoints(ctx, later); err != nil {
		return running, err
	}
	for _, p := range later {
		if err := os.RemoveAll(m.pointDir(id, p)); err != nil {
			log.Printf("sandbox: %s: point %s: remove: %v", id, p, err)
		}
	}
	// The restored VM counts dirty pages from this point.
	if err := m.cfg.Store.SetTimelineDiffOK(ctx, id, true); err != nil {
		return running, err
	}
	return running, m.enforcePointLimits(ctx, id, tpl)
}

// ForkPoint restores count new sandboxes from one point, and the origin does not
// change. The copies read an unlisted image made of links to the point's files,
// so they outlive the point, and a merge never writes to a file they read.
func (m *Manager) ForkPoint(ctx context.Context, id, pointID string, count int, allowSecret bool, metadata map[string]any, credentials []CredentialInput) ([]store.Sandbox, error) {
	if count < 1 {
		return nil, invalidf("count must be greater than zero")
	}
	unlock := m.lockOps(id)
	defer unlock()
	points, at, err := m.findPoint(ctx, id, pointID)
	if err != nil {
		return nil, err
	}
	point := points[at]
	if point.SecretBearing && !allowSecret {
		return nil, store.ErrConflict
	}
	row, err := m.cfg.Store.GetSandbox(ctx, id)
	if err != nil {
		return nil, err
	}
	tpl, err := m.cfg.Store.GetTemplate(ctx, row.TemplateName)
	if err != nil {
		return nil, err
	}
	allow := point.EgressAllow
	if allow == nil {
		allow = tpl.EgressAllow
	}
	sealed, err := m.sealCopyCredentials(credentials, tpl, allow)
	if err != nil {
		return nil, err
	}
	if metadata != nil {
		if row.Metadata, err = encodeMetadata(metadata); err != nil {
			return nil, err
		}
	}
	if err := m.fullPoint(context.WithoutCancel(ctx), points, at); err != nil {
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
	src := m.pointDir(id, pointID)
	for _, name := range []string{"mem", "state", runtime.OverlayFile} {
		if err := linkOrCopy(context.WithoutCancel(ctx), filepath.Join(src, name), filepath.Join(dir, name)); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
	}
	if err := m.cfg.Store.CreateSnapshot(ctx, store.Snapshot{
		ID:              snapID,
		TemplateName:    row.TemplateName,
		ParentID:        row.SnapshotID,
		SizeBytes:       allocated(dir),
		CreatedAt:       m.now(),
		SecretBearing:   point.SecretBearing,
		Listed:          false,
		OriginSandboxID: row.ID,
		EgressAllow:     point.EgressAllow,
	}); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	writeParentFile(dir, row.SnapshotID, row.TemplateName)
	img := image{
		dir: dir, template: tpl, snapshotID: snapID,
		secretBearing: point.SecretBearing,
		egress:        point.EgressAllow, bindings: sealed,
	}
	copies, err := m.makeCopies(ctx, img, row, count)
	if err != nil {
		m.reapSnapshot(context.WithoutCancel(ctx), snapID)
		return nil, err
	}
	return copies, nil
}

// linkOrCopy shares a file that nothing writes to. Roots on two filesystems copy.
func linkOrCopy(ctx context.Context, src, dst string) error {
	err := os.Link(src, dst)
	if errors.Is(err, syscall.EXDEV) {
		return runtime.CopySparse(ctx, src, dst)
	}
	return err
}
