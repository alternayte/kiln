package template

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// maxSymlinkDepth bounds symlink resolution, as image tools do.
const maxSymlinkDepth = 40

// applyLayer applies one uncompressed layer tar to root. Entry names are
// relative to the root. Whiteouts remove lower-layer entries.
func applyLayer(root string, tr *tar.Reader) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(hdr.Name)
		if name == "." || name == "" {
			continue
		}
		if strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("layer entry %q is not relative to the root", hdr.Name)
		}
		base := path.Base(name)
		switch {
		case base == ".wh..wh..opq":
			if err := removeDirContents(root, path.Dir(name)); err != nil {
				return err
			}
			continue
		case strings.HasPrefix(base, ".wh."):
			if err := removePath(root, path.Join(path.Dir(name), strings.TrimPrefix(base, ".wh."))); err != nil {
				return err
			}
			continue
		}
		if err := applyEntry(root, name, hdr, tr); err != nil {
			return err
		}
	}
}

func applyEntry(root, name string, hdr *tar.Header, body io.Reader) error {
	dir, base, err := parentDir(root, name)
	if err != nil {
		return err
	}
	target := filepath.Join(dir, base)
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := replace(target, true); err != nil {
			return err
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(target, modeOf(hdr)); err != nil {
			return err
		}
	case tar.TypeReg:
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := replace(target, false); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, body); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := os.Chmod(target, modeOf(hdr)); err != nil {
			return err
		}
	case tar.TypeSymlink:
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := replace(target, false); err != nil {
			return err
		}
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return err
		}
	case tar.TypeLink:
		linkTarget, err := resolveInRoot(root, path.Clean(hdr.Linkname), 0)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(linkTarget); err != nil {
			return fmt.Errorf("layer hardlink %q: target %q: %w", name, hdr.Linkname, err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := replace(target, false); err != nil {
			return err
		}
		if err := os.Link(linkTarget, target); err != nil {
			return err
		}
	case tar.TypeFifo:
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := replace(target, false); err != nil {
			return err
		}
		if err := unix.Mkfifo(target, uint32(modeOf(hdr))); err != nil {
			return err
		}
	case tar.TypeChar, tar.TypeBlock:
		if os.Geteuid() != 0 {
			return fmt.Errorf("layer %s %q needs root to create", typeName(hdr.Typeflag), name)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := replace(target, false); err != nil {
			return err
		}
		mode := uint32(modeOf(hdr)) | unix.S_IFCHR
		if hdr.Typeflag == tar.TypeBlock {
			mode = uint32(modeOf(hdr)) | unix.S_IFBLK
		}
		dev := int(unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor)))
		if err := unix.Mknod(target, mode, dev); err != nil {
			return err
		}
	default:
		return fmt.Errorf("layer entry %q has unsupported type %q", name, typeName(hdr.Typeflag))
	}
	if os.Geteuid() == 0 {
		if err := os.Lchown(target, hdr.Uid, hdr.Gid); err != nil {
			return err
		}
	}
	return nil
}

// replace removes an existing entry so the layer wins. Directories are kept
// so their children stay, unless keep means a plain directory is wanted.
func replace(target string, keepDir bool) error {
	fi, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if keepDir && fi.IsDir() {
		return nil
	}
	return os.RemoveAll(target)
}

func removePath(root, name string) error {
	dir, base, err := parentDir(root, name)
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(dir, base))
}

func removeDirContents(root, name string) error {
	dir, err := resolveInRoot(root, name, 0)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// parentDir resolves the parent of name inside root, following symlinks that
// stay inside root, and returns the resolved directory and the base name.
func parentDir(root, name string) (string, string, error) {
	dir, err := resolveInRoot(root, path.Dir(name), 0)
	if err != nil {
		return "", "", err
	}
	return dir, path.Base(name), nil
}

// resolveInRoot resolves a root-relative path, following symlinks while they
// stay inside root. A missing tail is returned unresolved; the caller
// creates it. An escape is an error.
func resolveInRoot(root, rel string, depth int) (string, error) {
	if depth > maxSymlinkDepth {
		return "", fmt.Errorf("template: too many symlinks resolving %q", rel)
	}
	rel = path.Clean(rel)
	if rel == "." || rel == "" {
		return root, nil
	}
	if strings.HasPrefix(rel, "/") || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("template: path %q escapes the rootfs", rel)
	}
	cur := root
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			// The tail does not exist yet. Return the full path so the
			// caller creates every missing part.
			return filepath.Join(append([]string{cur}, parts[i+1:]...)...), nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		link, err := os.Readlink(cur)
		if err != nil {
			return "", err
		}
		var next string
		if path.IsAbs(link) {
			next = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(link, "/")))
		} else {
			next = filepath.Join(filepath.Dir(cur), filepath.FromSlash(link))
		}
		next = filepath.Clean(next)
		if next != root && !strings.HasPrefix(next, root+string(filepath.Separator)) {
			return "", fmt.Errorf("template: path %q crosses a symlink out of the rootfs", rel)
		}
		rest := strings.TrimPrefix(next, root)
		rest = strings.TrimPrefix(rest, string(filepath.Separator))
		return resolveInRoot(root, rest, depth+1)
	}
	return cur, nil
}

func modeOf(hdr *tar.Header) os.FileMode {
	return os.FileMode(hdr.Mode & 0o777)
}

func typeName(t byte) string {
	switch t {
	case tar.TypeDir:
		return "directory"
	case tar.TypeReg:
		return "file"
	case tar.TypeSymlink:
		return "symlink"
	case tar.TypeLink:
		return "hardlink"
	case tar.TypeChar:
		return "character device"
	case tar.TypeBlock:
		return "block device"
	case tar.TypeFifo:
		return "fifo"
	default:
		return string(t)
	}
}
