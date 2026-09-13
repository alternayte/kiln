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
	if count != 1 {
		t.Fatalf("schema_migrations rows %d, want 1", count)
	}
}
