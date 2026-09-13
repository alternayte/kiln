// Package p1gate holds the P1 boot-and-exec gate. The gate itself is behind
// the kvm build tag; the rootfs builder stays portable so it compiles in the
// fast checks.
package p1gate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BuildRootfs builds the test rootfs at outPath. The rootfs holds kilninit as
// PID 1 and busybox applets for the test commands.
func BuildRootfs(ctx context.Context, repoRoot, outPath string) error {
	const busybox = "/bin/busybox"
	if _, err := os.Stat(busybox); err != nil {
		return fmt.Errorf("busybox is required at %s (install busybox-static): %w", busybox, err)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(outPath), ".rootfs-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	for _, d := range []string{"bin", "dev", "proc", "sys", "tmp", "run", "root", "etc"} {
		if err := os.MkdirAll(filepath.Join(staging, d), 0o755); err != nil {
			return err
		}
	}

	kilninit := filepath.Join(staging, "kilninit")
	build := exec.CommandContext(ctx, "go", "build", "-o", kilninit, "./guest/kilninit")
	build.Dir = repoRoot
	build.Env = append(withoutEnv(os.Environ(), "GOOS", "GOARCH", "CGO_ENABLED"),
		"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build kilninit: %v: %s", err, out)
	}

	busyboxDst := filepath.Join(staging, "bin", "busybox")
	if err := copyExec(busybox, busyboxDst); err != nil {
		return err
	}
	list := exec.CommandContext(ctx, busybox, "--list")
	out, err := list.Output()
	if err != nil {
		return fmt.Errorf("busybox --list: %w", err)
	}
	for _, applet := range strings.Fields(string(out)) {
		link := filepath.Join(staging, "bin", applet)
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink("busybox", link); err != nil {
			return err
		}
	}

	image := exec.CommandContext(ctx, "mke2fs", "-q", "-F", "-t", "ext4", "-d", staging, "-L", "kiln", outPath, "128M")
	if out, err := image.CombinedOutput(); err != nil {
		return fmt.Errorf("mke2fs: %v: %s", err, out)
	}
	return os.Chmod(outPath, 0o644)
}

func withoutEnv(env []string, keys ...string) []string {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k+"="] = true
	}
	out := env[:0:0]
	for _, e := range env {
		skip := false
		for prefix := range drop {
			if strings.HasPrefix(e, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, e)
		}
	}
	return out
}

func copyExec(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
