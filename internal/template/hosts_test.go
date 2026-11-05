package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alternayte/kiln/internal/guestproto"
)

// TestEnsureHostsKeepsTheImageFile pins issue 29: an image with no /etc/hosts
// gets the localhost lines and the marker, and the file of an image that holds
// one stays as it is.
func TestEnsureHostsKeepsTheImageFile(t *testing.T) {
	bare := t.TempDir()
	if err := ensureHosts(bare); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(bare, "etc", "hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), guestproto.HostsMarker+"\n127.0.0.1\tlocalhost\n::1\tlocalhost") {
		t.Errorf("written hosts:\n%s", got)
	}

	held := t.TempDir()
	own := "10.0.0.1 db\n"
	if err := os.MkdirAll(filepath.Join(held, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(held, "etc", "hosts"), []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureHosts(held); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(held, "etc", "hosts")); string(got) != own {
		t.Errorf("the image file changed to:\n%s", got)
	}
}
