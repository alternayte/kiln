package sandbox

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/alternayte/kiln/internal/store"
)

// TestPreviewLabel pins the shape of a hostname stem: 32 hex characters,
// with a port number appended after the first published port.
func TestPreviewLabel(t *testing.T) {
	const stem = "0123456789abcdef0123456789abcdef"
	cases := []struct {
		subdomain string
		want      string
		ok        bool
	}{
		{stem, stem, true},
		{stem + "-8001", stem, true},
		{stem + "-65535", stem, true},
		{stem + "-", "", false},
		{stem + "-abc", "", false},
		{stem[:31], "", false},
		{"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", "", false},
	}
	for _, tc := range cases {
		got, ok := previewLabel(tc.subdomain)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("previewLabel(%q) = %q, %t; want %q, %t", tc.subdomain, got, ok, tc.want, tc.ok)
		}
	}
}

// TestPublishedPortsShareOneStem proves the second port of a sandbox carries
// the first port's random stem and its port number.
func TestPublishedPortsShareOneStem(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "kiln.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	if err := st.CreateTemplate(ctx, store.Template{
		Name: "py", ImageRef: "img", VCPUs: 1, MemoryMB: 256, DiskMB: 1024,
		EgressAllow: []string{}, State: store.TemplateReady, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSandbox(ctx, store.Sandbox{
		ID: "one", TemplateName: "py", Lifecycle: store.LifecyclePersistent, State: store.SandboxRunning,
		IdleSeconds: 60, LastActiveAt: now, Metadata: "{}", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	mgr := New(Config{Store: st, Zone: "example.com"})

	first, err := mgr.Publish(ctx, "one", 8000, store.VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Subdomain) != 32 {
		t.Fatalf("first hostname %q, want a bare 32-character stem", first.Subdomain)
	}
	second, err := mgr.Publish(ctx, "one", 8001, store.VisibilityTeam)
	if err != nil {
		t.Fatal(err)
	}
	if want := first.Subdomain + "-8001"; second.Subdomain != want {
		t.Fatalf("second hostname %q, want %q", second.Subdomain, want)
	}
	// Republishing a port returns the same hostname.
	again, err := mgr.Publish(ctx, "one", 8001, store.VisibilityTeam)
	if err != nil {
		t.Fatal(err)
	}
	if again.Subdomain != second.Subdomain {
		t.Fatalf("republish moved the hostname: %q then %q", second.Subdomain, again.Subdomain)
	}
}
