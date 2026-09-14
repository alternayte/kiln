package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func openTest(t *testing.T) (Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kiln.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func sample(name string) Template {
	return Template{
		Name:        name,
		ImageRef:    "docker.io/library/python:3.12-slim",
		ImageDigest: "sha256:abc",
		VCPUs:       2,
		MemoryMB:    512,
		DiskMB:      4096,
		EgressAllow: []string{"pypi.org", "files.pythonhosted.org"},
		State:       TemplateBuilding,
		CreatedAt:   time.Unix(1700000000, 0).UTC(),
	}
}

func TestTemplateRoundTrip(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	want := sample("py312")
	if err := s.CreateTemplate(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTemplate(ctx, "py312")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestTemplateNotFound(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if _, err := s.GetTemplate(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTemplate error %v, want ErrNotFound", err)
	}
	if err := s.SetTemplateState(ctx, "absent", TemplateReady, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetTemplateState error %v, want ErrNotFound", err)
	}
	if err := s.DeleteTemplate(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteTemplate error %v, want ErrNotFound", err)
	}
}

func TestSetTemplateState(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.CreateTemplate(ctx, sample("py312")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTemplateState(ctx, "py312", TemplateFailed, "setup failed"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTemplate(ctx, "py312")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != TemplateFailed || got.Error != "setup failed" {
		t.Fatalf("state %q error %q", got.State, got.Error)
	}
	if err := s.SetTemplateState(ctx, "py312", TemplateReady, ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetTemplate(ctx, "py312")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != TemplateReady || got.Error != "" {
		t.Fatalf("state %q error %q after ready", got.State, got.Error)
	}
}

func TestListAndDeleteTemplates(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	for _, name := range []string{"beta", "alpha"} {
		if err := s.CreateTemplate(ctx, sample(name)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "beta" {
		t.Fatalf("list %+v", list)
	}
	if err := s.DeleteTemplate(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "beta" {
		t.Fatalf("list after delete %+v", list)
	}
}

func TestDuplicateTemplateFails(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.CreateTemplate(ctx, sample("py312")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTemplate(ctx, sample("py312")); err == nil {
		t.Fatal("duplicate template name accepted")
	}
}

func TestNilEgressIsStoredAsEmptyArray(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	tmpl := sample("empty")
	tmpl.EgressAllow = nil
	if err := s.CreateTemplate(ctx, tmpl); err != nil {
		t.Fatal(err)
	}
	sq := s.(*sqliteStore)
	var raw string
	if err := sq.db.QueryRowContext(ctx, `SELECT egress_allow FROM templates WHERE name = ?`, "empty").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "[]" {
		t.Fatalf("egress_allow %q, want []", raw)
	}
	got, err := s.GetTemplate(ctx, "empty")
	if err != nil {
		t.Fatal(err)
	}
	if got.EgressAllow == nil || len(got.EgressAllow) != 0 {
		t.Fatalf("EgressAllow %#v, want empty", got.EgressAllow)
	}
}

func TestEvents(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	at := time.Unix(1700000000, 0).UTC()
	if err := s.AppendEvent(ctx, Event{FromState: TemplateBuilding, ToState: TemplateReady, Reason: "built", At: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, Event{SandboxID: "sbx-1", FromState: "created", ToState: "running", At: at}); err != nil {
		t.Fatal(err)
	}
	global, err := s.ListEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(global) != 1 || global[0].ToState != TemplateReady || global[0].SandboxID != "" {
		t.Fatalf("global events %+v", global)
	}
	sandbox, err := s.ListEvents(ctx, "sbx-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sandbox) != 1 || sandbox[0].FromState != "created" {
		t.Fatalf("sandbox events %+v", sandbox)
	}
}

func TestReopenAppliesMigrationsOnce(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	if err := s.CreateTemplate(ctx, sample("py312")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if _, err := again.GetTemplate(ctx, "py312"); err != nil {
		t.Fatalf("row did not survive reopen: %v", err)
	}
	sq := again.(*sqliteStore)
	var count int
	if err := sq.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("schema_migrations rows %d, want 3", count)
	}
}

func TestStartTemplateBuildStateMachine(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.StartTemplateBuild(ctx, sample("py312")); err != nil {
		t.Fatal(err)
	}
	if err := s.StartTemplateBuild(ctx, sample("py312")); !errors.Is(err, ErrConflict) {
		t.Fatalf("second build error %v, want ErrConflict", err)
	}
	if err := s.SetTemplateState(ctx, "py312", TemplateReady, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.StartTemplateBuild(ctx, sample("py312")); !errors.Is(err, ErrConflict) {
		t.Fatalf("build over ready error %v, want ErrConflict", err)
	}
	if err := s.SetTemplateState(ctx, "py312", TemplateFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := s.StartTemplateBuild(ctx, sample("py312")); err != nil {
		t.Fatalf("failed template refused a rebuild: %v", err)
	}
	got, err := s.GetTemplate(ctx, "py312")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != TemplateBuilding || got.Error != "" {
		t.Fatalf("state %q error %q after rebuild", got.State, got.Error)
	}
}

func TestSetTemplateDigest(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.CreateTemplate(ctx, sample("py312")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTemplateDigest(ctx, "py312", "sha256:def"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTemplate(ctx, "py312")
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageDigest != "sha256:def" {
		t.Fatalf("digest %q", got.ImageDigest)
	}
	if err := s.SetTemplateDigest(ctx, "absent", "sha256:def"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error %v, want ErrNotFound", err)
	}
}

func TestAllocateCIDPicksTheLowestFree(t *testing.T) {
	if cid, err := allocateCID(map[int]bool{3: true, 5: true}, 6); err != nil || cid != 4 {
		t.Fatalf("allocateCID = %d, %v; want 4", cid, err)
	}
	if _, err := allocateCID(map[int]bool{3: true, 4: true, 5: true, 6: true}, 6); !errors.Is(err, ErrExhausted) {
		t.Fatalf("exhausted pool error %v, want ErrExhausted", err)
	}
}

func TestCreateSandboxAllocatesDistinctCIDs(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.CreateTemplate(ctx, sample("py312")); err != nil {
		t.Fatal(err)
	}
	first, err := s.CreateSandbox(ctx, sandboxRow("one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateSandbox(ctx, sandboxRow("two"))
	if err != nil {
		t.Fatal(err)
	}
	if first.VsockCID == nil || second.VsockCID == nil {
		t.Fatalf("cids %v and %v, want values", first.VsockCID, second.VsockCID)
	}
	if *first.VsockCID != MinVsockCID || *second.VsockCID != MinVsockCID+1 {
		t.Fatalf("cids %d and %d, want %d and %d", *first.VsockCID, *second.VsockCID, MinVsockCID, MinVsockCID+1)
	}
	if err := s.DestroySandbox(ctx, first.ID, time.Unix(1700000000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	// The destroyed row frees its CID, so the pool hands the lowest free one out.
	third, err := s.CreateSandbox(ctx, sandboxRow("three"))
	if err != nil {
		t.Fatal(err)
	}
	if third.VsockCID == nil || *third.VsockCID != MinVsockCID {
		t.Fatalf("reused cid %v, want %d", third.VsockCID, MinVsockCID)
	}
}

func TestSnapshotListingHidesUnlistedImages(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.CreateTemplate(ctx, sample("py312")); err != nil {
		t.Fatal(err)
	}
	created := time.Unix(1700000000, 0).UTC()
	listed := Snapshot{ID: "listed", TemplateName: "py312", SizeBytes: 10, CreatedAt: created, Listed: true, OriginSandboxID: "one"}
	unlisted := Snapshot{ID: "unlisted", TemplateName: "py312", SizeBytes: 20, CreatedAt: created, Listed: false, OriginSandboxID: "one"}
	for _, snap := range []Snapshot{listed, unlisted} {
		if err := s.CreateSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "listed" {
		t.Fatalf("list %+v, want only the listed image", got)
	}
	if _, err := s.GetSnapshot(ctx, "unlisted"); err != nil {
		t.Fatalf("get unlisted: %v", err)
	}

	child := sandboxRow("child")
	child.SnapshotID = "unlisted"
	if _, err := s.CreateSandbox(ctx, child); err != nil {
		t.Fatal(err)
	}
	live, err := s.CountRestoredChildren(ctx, "unlisted")
	if err != nil || live != 1 {
		t.Fatalf("live children %d, %v; want 1", live, err)
	}
	if err := s.DestroySandbox(ctx, "child", created); err != nil {
		t.Fatal(err)
	}
	live, err = s.CountRestoredChildren(ctx, "unlisted")
	if err != nil || live != 0 {
		t.Fatalf("live children %d, %v; want 0", live, err)
	}
	// The destroyed child keeps its row, so the snapshot delete must clear
	// the reference before the foreign key lets the image row go.
	if err := s.DeleteSnapshot(ctx, "unlisted"); err != nil {
		t.Fatalf("delete snapshot with a destroyed child: %v", err)
	}
	if _, err := s.GetSnapshot(ctx, "unlisted"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v, want ErrNotFound", err)
	}
}

// sandboxRow is a minimal row for the pool tests.
func sandboxRow(id string) Sandbox {
	return Sandbox{
		ID:           id,
		TemplateName: "py312",
		Lifecycle:    LifecycleEphemeral,
		State:        SandboxRunning,
		IdleSeconds:  60,
		LastActiveAt: time.Unix(1700000000, 0).UTC(),
		Metadata:     "{}",
		CreatedAt:    time.Unix(1700000000, 0).UTC(),
	}
}

func TestTemplateDependentsWithoutChildTables(t *testing.T) {
	s, _ := openTest(t)
	live, snapshots, err := s.TemplateDependents(context.Background(), "py312")
	if err != nil {
		t.Fatal(err)
	}
	if live != 0 || snapshots != 0 {
		t.Fatalf("dependents %d live, %d snapshots, want 0 and 0", live, snapshots)
	}
}

func TestTemplateDependentsCountsChildren(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	sq := s.(*sqliteStore)
	if err := s.CreateTemplate(ctx, sample("py312")); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO sandboxes (
		id, template_name, lifecycle, state, idle_seconds, last_active_at, metadata, created_at, destroyed_at
	) VALUES (?, 'py312', 'ephemeral', 'running', 60, 0, '{}', 0, ?)`
	if _, err := sq.db.ExecContext(ctx, insert, "live", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.db.ExecContext(ctx, insert, "dead", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.db.ExecContext(ctx, `INSERT INTO snapshots (id, template_name, size_bytes, created_at) VALUES ('snap', 'py312', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	live, snapshots, err := s.TemplateDependents(ctx, "py312")
	if err != nil {
		t.Fatal(err)
	}
	if live != 1 || snapshots != 1 {
		t.Fatalf("dependents %d live, %d snapshots, want 1 and 1", live, snapshots)
	}
	live, snapshots, err = s.TemplateDependents(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	if live != 0 || snapshots != 0 {
		t.Fatalf("other template: %d live, %d snapshots, want 0 and 0", live, snapshots)
	}
}
