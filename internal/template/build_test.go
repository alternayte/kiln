package template

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestBuildRequestValidate(t *testing.T) {
	ok := BuildRequest{
		Name:     "py312",
		Image:    "docker.io/library/python:3.12-slim",
		VCPUs:    2,
		MemoryMB: 512,
		DiskMB:   4096,
	}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*BuildRequest)
	}{
		{"name", func(r *BuildRequest) { r.Name = "../escape" }},
		{"empty name", func(r *BuildRequest) { r.Name = "" }},
		{"image", func(r *BuildRequest) { r.Image = " " }},
		{"vcpus", func(r *BuildRequest) { r.VCPUs = 0 }},
		{"memory", func(r *BuildRequest) { r.MemoryMB = 0 }},
		{"disk", func(r *BuildRequest) { r.DiskMB = 0 }},
		{"setup", func(r *BuildRequest) { r.Setup = []string{"ok", " "} }},
		{"egress", func(r *BuildRequest) { r.EgressAllow = []string{"bad/name"} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := ok
			c.change(&r)
			err := r.Validate()
			if err == nil {
				t.Fatal("request accepted")
			}
			if !IsInvalid(err) {
				t.Fatalf("error %v, want invalid", err)
			}
		})
	}
}

func TestBuildRequestAllowsEmptyEgress(t *testing.T) {
	r := BuildRequest{Name: "n", Image: "i", VCPUs: 1, MemoryMB: 1, DiskMB: 1}
	if err := r.Validate(); err != nil {
		t.Fatalf("empty egress rejected: %v", err)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Manifest{
		Name:        "py312",
		ImageRef:    "docker.io/library/python:3.12-slim",
		ImageDigest: "sha256:abc",
		VCPUs:       2,
		MemoryMB:    512,
		DiskMB:      4096,
		EgressAllow: []string{"pypi.org", "files.pythonhosted.org"},
		Setup:       []string{"pip install requests"},
		CreatedAt:   time.Unix(1700000000, 0).UTC(),
	}
	if err := writeManifest(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest %+v, want %+v", got, want)
	}
}

func TestWriteTreeFileDoesNotEscape(t *testing.T) {
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	root := filepath.Join(parent, "root")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	// A relative symlink that leaves the rootfs is refused.
	if err := os.Symlink(filepath.Join("..", "outside"), filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}
	if err := writeTreeFile(root, "etc/resolv.conf", []byte("x"), 0o644); err == nil {
		t.Fatal("write through an escaping symlink was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "resolv.conf")); !os.IsNotExist(err) {
		t.Fatalf("file landed outside the rootfs: %v", err)
	}

	// An absolute symlink resolves against the rootfs, as it would in the
	// guest, and never against the host root. os.RemoveAll clears it.
	if err := os.Remove(filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/outside", filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}
	if err := writeTreeFile(root, "etc/resolv.conf", []byte("x"), 0o644); err != nil {
		t.Fatalf("absolute symlink: %v", err)
	}
	if _, err := os.Stat("/outside/resolv.conf"); !os.IsNotExist(err) {
		t.Fatalf("file landed on the host root: %v", err)
	}
}

func TestWriteTreeFileReplacesSymlink(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "etc", "resolv.conf")); err != nil {
		t.Fatal(err)
	}
	if err := writeTreeFile(root, "etc/resolv.conf", []byte("nameserver 172.31.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "keep" {
		t.Fatalf("victim %q err %v", got, err)
	}
	fi, err := os.Lstat(filepath.Join(root, "etc", "resolv.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("resolv.conf is still a symlink")
	}
}

func TestInjectKilninitReplacesSymlink(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "kilninit")
	if err := os.WriteFile(src, []byte("guest-init"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(victim, filepath.Join(root, "kilninit")); err != nil {
		t.Fatal(err)
	}
	if err := injectKilninit(root, src); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "keep" {
		t.Fatalf("victim %q err %v", got, err)
	}
	if got := readFile(t, filepath.Join(root, "kilninit")); got != "guest-init" {
		t.Fatalf("kilninit %q", got)
	}
}
