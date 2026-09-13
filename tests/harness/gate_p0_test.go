//go:build kvm

package harness

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGateP0 proves the harness catches the failures it exists for: a
// preflight that does not fail closed, a leak check that misses a stray
// device, a status that names the wrong phase, and a hook that lets a
// frozen-spec edit or a skipped test through.
func TestGateP0(t *testing.T) {
	root := repoRoot(t)

	t.Run("PreflightPassesWithPinnedFakes", func(t *testing.T) {
		v := versions(t, root)
		bin := fakeBin(t, map[string]string{
			"firecracker": "#!/bin/sh\necho \"Firecracker " + v["FIRECRACKER_VERSION"] + "\"\n",
			"jailer":      "#!/bin/sh\nexit 0\n",
			"nft":         "#!/bin/sh\nexit 0\n",
			"mke2fs":      "#!/bin/sh\nexit 0\n",
		})
		r := run(t, root, preflightEnv(t, bin, kvmFile(t)), "bash", "scripts/preflight.sh")
		if r.code != 0 {
			t.Fatalf("preflight exit %d, want 0:\n%s", r.code, r.out)
		}
	})

	t.Run("PreflightFailsWithoutKVM", func(t *testing.T) {
		v := versions(t, root)
		bin := fakeBin(t, map[string]string{
			"firecracker": "#!/bin/sh\necho \"Firecracker " + v["FIRECRACKER_VERSION"] + "\"\n",
			"jailer":      "#!/bin/sh\nexit 0\n",
			"nft":         "#!/bin/sh\nexit 0\n",
			"mke2fs":      "#!/bin/sh\nexit 0\n",
		})
		r := run(t, root, preflightEnv(t, bin, filepath.Join(t.TempDir(), "no-kvm")), "bash", "scripts/preflight.sh")
		if r.code == 0 {
			t.Fatalf("preflight exit 0 without KVM:\n%s", r.out)
		}
		if !strings.Contains(r.out, "KVM") {
			t.Fatalf("preflight did not report the missing KVM:\n%s", r.out)
		}
	})

	t.Run("PreflightFailsOnMissingBinary", func(t *testing.T) {
		v := versions(t, root)
		bin := fakeBin(t, map[string]string{
			"firecracker": "#!/bin/sh\necho \"Firecracker " + v["FIRECRACKER_VERSION"] + "\"\n",
			"jailer":      "#!/bin/sh\nexit 0\n",
			"mke2fs":      "#!/bin/sh\nexit 0\n",
		})
		r := run(t, root, preflightEnv(t, bin, kvmFile(t)), "bash", "scripts/preflight.sh")
		if r.code == 0 {
			t.Fatalf("preflight exit 0 without nft:\n%s", r.out)
		}
		if !strings.Contains(r.out, "nft") {
			t.Fatalf("preflight did not report the missing nft:\n%s", r.out)
		}
	})

	t.Run("PreflightFailsOnVersionMismatch", func(t *testing.T) {
		bin := fakeBin(t, map[string]string{
			"firecracker": "#!/bin/sh\necho \"Firecracker v0.0.0\"\n",
			"jailer":      "#!/bin/sh\nexit 0\n",
			"nft":         "#!/bin/sh\nexit 0\n",
			"mke2fs":      "#!/bin/sh\nexit 0\n",
		})
		r := run(t, root, preflightEnv(t, bin, kvmFile(t)), "bash", "scripts/preflight.sh")
		if r.code == 0 {
			t.Fatalf("preflight exit 0 with a wrong firecracker version:\n%s", r.out)
		}
		if !strings.Contains(r.out, "v0.0.0") {
			t.Fatalf("preflight did not report the version mismatch:\n%s", r.out)
		}
	})

	t.Run("LeakPassesCleanCommand", func(t *testing.T) {
		bin := fakeIPBin(t)
		marker := filepath.Join(t.TempDir(), "marker")
		r := run(t, root, leakEnv(t, bin, marker), "bash", "scripts/leak.sh", "bash", "-c", "exit 0")
		if r.code != 0 {
			t.Fatalf("leak.sh exit %d on a clean command:\n%s", r.code, r.out)
		}
		if !strings.Contains(r.out, "leak check: clean") {
			t.Fatalf("leak.sh did not report clean:\n%s", r.out)
		}
	})

	t.Run("LeakDetectsStrayTap", func(t *testing.T) {
		bin := fakeIPBin(t)
		marker := filepath.Join(t.TempDir(), "marker")
		r := run(t, root, leakEnv(t, bin, marker), "bash", "scripts/leak.sh", "bash", "-c", `: > "$KILN_TEST_LEAK_MARKER"`)
		if r.code == 0 {
			t.Fatalf("leak.sh exit 0 with a stray kiln- TAP:\n%s", r.out)
		}
		if !strings.Contains(r.out, "LEAK DETECTED") {
			t.Fatalf("leak.sh did not report the stray TAP:\n%s", r.out)
		}
	})

	t.Run("StatusNamesFirstUntaggedGate", func(t *testing.T) {
		r := run(t, root, nil, "just", "status")
		if r.code != 0 {
			t.Fatalf("just status exit %d:\n%s", r.code, r.out)
		}
		want := firstUntaggedGate(t, root)
		if !strings.Contains(r.out, "next: "+want) {
			t.Fatalf("just status does not name %s as next:\n%s", want, r.out)
		}
	})

	t.Run("HookAllowsCleanCommit", func(t *testing.T) {
		dir := seedRepo(t, root)
		writeFile(t, filepath.Join(dir, "notes.txt"), "clean\n")
		gitAdd(t, dir, "notes.txt")
		if r := run(t, dir, nil, "git", "commit", "-q", "-m", "clean"); r.code != 0 {
			t.Fatalf("clean commit was refused:\n%s", r.out)
		}
	})

	t.Run("HookRefusesSpecChange", func(t *testing.T) {
		dir := seedRepo(t, root)
		appendLine(t, filepath.Join(dir, "docs", "internal", "sdd.md"), "edit")
		gitAdd(t, dir, "docs/internal/sdd.md")
		r := run(t, dir, nil, "git", "commit", "-q", "-m", "edit spec")
		if r.code == 0 {
			t.Fatal("commit touching docs/internal/sdd.md passed; want refused")
		}
		if !strings.Contains(r.out, "REJECTED") {
			t.Fatalf("refusal did not say REJECTED:\n%s", r.out)
		}
	})

	t.Run("HookAllowsSpecChangeWithOverride", func(t *testing.T) {
		dir := seedRepo(t, root)
		appendLine(t, filepath.Join(dir, "docs", "internal", "sdd.md"), "edit")
		gitAdd(t, dir, "docs/internal/sdd.md")
		r := run(t, dir, envWith("KILN_SPEC_CHANGE", "1"), "git", "commit", "-q", "-m", "edit spec")
		if r.code != 0 {
			t.Fatalf("commit with KILN_SPEC_CHANGE=1 was refused:\n%s", r.out)
		}
	})

	t.Run("HookRefusesSkippedTest", func(t *testing.T) {
		dir := seedRepo(t, root)
		writeFile(t, filepath.Join(dir, "x_test.go"), "package x\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {\n\t"+"t."+"Skip(\"not ready\")\n}\n")
		gitAdd(t, dir, "x_test.go")
		r := run(t, dir, nil, "git", "commit", "-q", "-m", "skipped test")
		if r.code == 0 {
			t.Fatal("commit with a skip call passed; want refused")
		}
		if !strings.Contains(r.out, "t."+"Skip") {
			t.Fatalf("refusal did not name the skip call:\n%s", r.out)
		}
	})
}

type result struct {
	out  string
	code int
}

func run(t *testing.T, dir string, env []string, name string, args ...string) result {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return result{out: string(out)}
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return result{out: string(out), code: exit.ExitCode()}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	r := run(t, "", nil, "git", "rev-parse", "--show-toplevel")
	if r.code != 0 {
		t.Fatalf("git rev-parse --show-toplevel: %s", r.out)
	}
	return strings.TrimSpace(r.out)
}

// envWith returns the process environment with each key replaced.
func envWith(kv ...string) []string {
	base := os.Environ()
	for i := 0; i+1 < len(kv); i += 2 {
		k, v := kv[i], kv[i+1]
		next := make([]string, 0, len(base)+1)
		for _, e := range base {
			if !strings.HasPrefix(e, k+"=") {
				next = append(next, e)
			}
		}
		base = append(next, k+"="+v)
	}
	return base
}

// preflightEnv gives preflight fake binaries and a file in place of KVM. PATH
// holds only the fakes, so a missing binary cannot hide in the system PATH.
// preflight uses no external commands itself.
func preflightEnv(t *testing.T, bin, kvm string) []string {
	t.Helper()
	return envWith("KILN_ROOT", t.TempDir(), "KILN_KVM_DEV", kvm, "PATH", bin)
}

func kvmFile(t *testing.T) string {
	t.Helper()
	kvm := filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(kvm, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return kvm
}

func leakEnv(t *testing.T, bin, marker string) []string {
	t.Helper()
	return envWith(
		"KILN_ROOT", t.TempDir(),
		"KILN_TEST_LEAK_MARKER", marker,
		"PATH", bin+":"+os.Getenv("PATH"),
	)
}

// fakeIPBin fakes `ip -o link show`. It prints a kiln- link only after the
// wrapped command creates the marker, so leak.sh sees one new device.
func fakeIPBin(t *testing.T) string {
	t.Helper()
	return fakeBin(t, map[string]string{
		"ip": "#!/bin/sh\nif [ -e \"$KILN_TEST_LEAK_MARKER\" ]; then\n  echo \"7: kiln-stray0: <BROADCAST,MULTICAST> mtu 1500\"\nfi\n",
	})
}

func fakeBin(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		writeFile(t, filepath.Join(dir, name), body)
		if err := os.Chmod(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func versions(t *testing.T, root string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "scripts", "versions.env"))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("versions.env: bad line %q", line)
		}
		m[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	for _, k := range []string{"ARCH", "FIRECRACKER_VERSION", "FIRECRACKER_URL", "FIRECRACKER_SHA256", "KERNEL_VERSION", "KERNEL_URL", "KERNEL_SHA256"} {
		if m[k] == "" {
			t.Fatalf("versions.env does not set %s", k)
		}
	}
	return m
}

func firstUntaggedGate(t *testing.T, root string) string {
	t.Helper()
	r := run(t, root, nil, "git", "tag", "-l", "gate/*")
	if r.code != 0 {
		t.Fatalf("git tag: %s", r.out)
	}
	tags := map[string]bool{}
	for _, tag := range strings.Fields(r.out) {
		tags[tag] = true
	}
	for _, g := range []string{"P0", "P1", "P2", "P3", "P4", "P5", "P6"} {
		if !tags["gate/"+g] {
			return g
		}
	}
	return "none"
}

// seedRepo is a throwaway git repo with the hook and its inputs. The seed
// commit runs no hook; the hook is enabled after it.
func seedRepo(t *testing.T, root string) string {
	t.Helper()
	dir := t.TempDir()
	copyPath(t, filepath.Join(root, "AGENTS.md"), filepath.Join(dir, "AGENTS.md"))
	cpTree(t, filepath.Join(root, "checks"), filepath.Join(dir, "checks"))
	cpTree(t, filepath.Join(root, ".githooks"), filepath.Join(dir, ".githooks"))
	if err := os.MkdirAll(filepath.Join(dir, "docs", "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyPath(t, filepath.Join(root, "docs", "internal", "sdd.md"), filepath.Join(dir, "docs", "internal", "sdd.md"))
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "gate"},
		{"config", "user.email", "gate@example.invalid"},
		{"config", "core.hooksPath", ".git/hooks"},
		{"add", "-A"},
		{"commit", "-q", "-m", "seed"},
		{"config", "core.hooksPath", ".githooks"},
	} {
		if r := run(t, dir, nil, "git", args...); r.code != 0 {
			t.Fatalf("git %v: exit %d\n%s", args, r.code, r.out)
		}
	}
	return dir
}

func gitAdd(t *testing.T, dir, path string) {
	t.Helper()
	if r := run(t, dir, nil, "git", "add", "--", path); r.code != 0 {
		t.Fatalf("git add %s: %s", path, r.out)
	}
}

func copyPath(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dst, string(b))
}

func cpTree(t *testing.T, src, dst string) {
	t.Helper()
	if r := run(t, "/", nil, "cp", "-R", src, dst); r.code != 0 {
		t.Fatalf("cp -R %s: %s", src, r.out)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
