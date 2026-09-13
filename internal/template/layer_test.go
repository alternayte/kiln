package template

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

type entry struct {
	name string
	typ  byte
	mode int64
	link string
	data string
}

func layerReader(t *testing.T, entries ...entry) *tar.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: mode, Linkname: e.link}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return tar.NewReader(&buf)
}

func apply(t *testing.T, root string, entries ...entry) error {
	t.Helper()
	return applyLayer(root, layerReader(t, entries...))
}

func mustApply(t *testing.T, root string, entries ...entry) {
	t.Helper()
	if err := apply(t, root, entries...); err != nil {
		t.Fatalf("applyLayer: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyLayerBasic(t *testing.T) {
	root := t.TempDir()
	mustApply(t, root,
		entry{name: "dir/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "dir/a.txt", typ: tar.TypeReg, mode: 0o640, data: "hello"},
		entry{name: "link", typ: tar.TypeSymlink, link: "dir/a.txt"},
		entry{name: "hard", typ: tar.TypeLink, link: "dir/a.txt"},
	)
	if got := readFile(t, filepath.Join(root, "dir", "a.txt")); got != "hello" {
		t.Fatalf("file content %q", got)
	}
	if target, err := os.Readlink(filepath.Join(root, "link")); err != nil || target != "dir/a.txt" {
		t.Fatalf("symlink target %q err %v", target, err)
	}
	a, err := os.Stat(filepath.Join(root, "dir", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := os.Stat(filepath.Join(root, "hard"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, h) {
		t.Fatal("hardlink is a different file")
	}
	if fi, err := os.Stat(filepath.Join(root, "dir", "a.txt")); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %o, want 640", fi.Mode().Perm())
	}
}

func TestParentDirectoriesAreCreated(t *testing.T) {
	root := t.TempDir()
	mustApply(t, root, entry{name: "a/b/c.txt", typ: tar.TypeReg, data: "x"})
	if got := readFile(t, filepath.Join(root, "a", "b", "c.txt")); got != "x" {
		t.Fatalf("content %q", got)
	}
}

func TestWhiteoutRemovesFile(t *testing.T) {
	root := t.TempDir()
	mustApply(t, root,
		entry{name: "keep.txt", typ: tar.TypeReg, data: "keep"},
		entry{name: "gone.txt", typ: tar.TypeReg, data: "gone"},
	)
	mustApply(t, root, entry{name: ".wh.gone.txt", typ: tar.TypeReg})
	if _, err := os.Stat(filepath.Join(root, "gone.txt")); !os.IsNotExist(err) {
		t.Fatalf("gone.txt still exists: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "keep.txt")); got != "keep" {
		t.Fatalf("keep.txt content %q", got)
	}
}

func TestOpaqueWhiteoutClearsDirectory(t *testing.T) {
	root := t.TempDir()
	mustApply(t, root,
		entry{name: "d/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "d/a", typ: tar.TypeReg, data: "a"},
		entry{name: "d/b", typ: tar.TypeReg, data: "b"},
		entry{name: "d/sub/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "d/sub/c", typ: tar.TypeReg, data: "c"},
	)
	mustApply(t, root,
		entry{name: "d/.wh..wh..opq", typ: tar.TypeReg},
		entry{name: "d/new", typ: tar.TypeReg, data: "new"},
	)
	for _, gone := range []string{"d/a", "d/b", "d/sub"} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", gone, err)
		}
	}
	if got := readFile(t, filepath.Join(root, "d", "new")); got != "new" {
		t.Fatalf("d/new content %q", got)
	}
}

func TestRejectsParentEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := apply(t, root, entry{name: "../evil", typ: tar.TypeReg, data: "x"}); err == nil {
		t.Fatal("parent escape accepted")
	}
	if _, err := os.Stat(filepath.Join(parent, "evil")); !os.IsNotExist(err) {
		t.Fatalf("evil file was written: %v", err)
	}
}

func TestRejectsAbsolutePath(t *testing.T) {
	root := t.TempDir()
	if err := apply(t, root, entry{name: "/etc/passwd", typ: tar.TypeReg, data: "x"}); err == nil {
		t.Fatal("absolute path accepted")
	}
}

func TestRejectsSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	mustApply(t, root, entry{name: "link", typ: tar.TypeSymlink, link: "../../outside"})
	if err := apply(t, root, entry{name: "link/pwn", typ: tar.TypeReg, data: "x"}); err == nil {
		t.Fatal("symlink escape accepted")
	}
	if _, err := os.Stat(filepath.Join(parent, "outside", "pwn")); !os.IsNotExist(err) {
		t.Fatalf("file was written outside the root: %v", err)
	}
}

func TestSymlinkInsideRootIsFollowed(t *testing.T) {
	root := t.TempDir()
	mustApply(t, root,
		entry{name: "real/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "link", typ: tar.TypeSymlink, link: "real"},
	)
	mustApply(t, root, entry{name: "link/file", typ: tar.TypeReg, data: "x"})
	if got := readFile(t, filepath.Join(root, "real", "file")); got != "x" {
		t.Fatalf("content %q", got)
	}
}

func TestLayerOverwritesEntry(t *testing.T) {
	root := t.TempDir()
	mustApply(t, root, entry{name: "f", typ: tar.TypeReg, data: "one"})
	mustApply(t, root, entry{name: "f", typ: tar.TypeSymlink, link: "other"})
	fi, err := os.Lstat(filepath.Join(root, "f"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("f is not a symlink after the second layer")
	}
}

func TestInjectKilninit(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "kilninit")
	if err := os.WriteFile(src, []byte("guest-init"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := injectKilninit(root, src); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "kilninit")
	if got := readFile(t, target); got != "guest-init" {
		t.Fatalf("content %q", got)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %o, want 755", fi.Mode().Perm())
	}
}
