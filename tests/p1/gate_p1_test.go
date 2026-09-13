//go:build kvm

package p1gate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/kiln/internal/guestproto"
	"github.com/alternayte/kiln/internal/runtime"
)

// TestGateP1 boots the test rootfs, execs commands, and shuts down. The gate
// recipe runs it under scripts/leak.sh. It runs 20 start-exec-stop cycles and
// asserts the exec contract.
func TestGateP1(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("gate P1 needs root: start it with sudo")
	}
	repo := repoRoot(t)
	runPreflight(t, repo)
	root := kilnRoot()
	kernel := kernelPath(t, repo, root)

	rootfs := filepath.Join(repo, "tests", "p1", "testdata", "rootfs.ext4")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := BuildRootfs(ctx, repo, rootfs); err != nil {
		t.Fatalf("build rootfs: %v", err)
	}

	rt, err := runtime.New(runtime.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("Cycles", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			vm := startVM(t, rt, kernel, rootfs, fmt.Sprintf("p1-cycle-%d", i))
			res := doExec(t, vm, runtime.ExecRequest{Cmd: []string{"echo", "hello"}})
			if res.ExitCode != 0 {
				t.Fatalf("cycle %d: exit %d, stderr %q", i, res.ExitCode, res.Stderr)
			}
			if res.Stdout != "hello\n" {
				t.Fatalf("cycle %d: stdout %q, want %q", i, res.Stdout, "hello\n")
			}
			stopVM(t, rt, vm)
		}
	})

	t.Run("EnvironmentAndCwd", func(t *testing.T) {
		vm := startVM(t, rt, kernel, rootfs, "p1-env")
		defer stopVM(t, rt, vm)
		res := doExec(t, vm, runtime.ExecRequest{
			Cmd: []string{"sh", "-c", "echo \"$FOO $HOME\"; pwd"},
			Cwd: "/tmp",
			Env: map[string]string{"FOO": "bar"},
		})
		if res.Stdout != "bar /root\n/tmp\n" {
			t.Fatalf("stdout %q, want %q", res.Stdout, "bar /root\n/tmp\n")
		}
	})

	t.Run("InvalidRequests", func(t *testing.T) {
		vm := startVM(t, rt, kernel, rootfs, "p1-invalid")
		defer stopVM(t, rt, vm)

		cases := map[string]runtime.ExecRequest{
			"no cmd":  {},
			"timeout": {Cmd: []string{"echo"}, TimeoutSeconds: guestproto.ExecTimeoutMax + 1},
			"bad cwd": {Cmd: []string{"echo", "hi"}, Cwd: "/nope"},
		}
		for name, req := range cases {
			_, err := vm.Exec(context.Background(), req)
			if !runtime.IsExecError(err, guestproto.CodeInvalid) {
				t.Fatalf("%s: err %v, want an invalid ExecError", name, err)
			}
		}
	})

	t.Run("TimeoutKillsProcessGroup", func(t *testing.T) {
		vm := startVM(t, rt, kernel, rootfs, "p1-timeout")
		defer stopVM(t, rt, vm)
		res := doExec(t, vm, runtime.ExecRequest{Cmd: []string{"sh", "-c", "sleep 30"}, TimeoutSeconds: 1})
		if !res.TimedOut {
			t.Fatalf("timed_out false, result %+v", res)
		}
		if res.ExitCode != -1 {
			t.Fatalf("exit %d, want -1", res.ExitCode)
		}
		ps := doExec(t, vm, runtime.ExecRequest{Cmd: []string{"ps"}})
		if strings.Contains(ps.Stdout, "sleep 30") {
			t.Fatalf("timed-out process group survived:\n%s", ps.Stdout)
		}
	})

	t.Run("ConcurrentExecs", func(t *testing.T) {
		vm := startVM(t, rt, kernel, rootfs, "p1-concurrent")
		defer stopVM(t, rt, vm)

		first := make(chan error, 1)
		go func() {
			_, err := vm.Exec(context.Background(), runtime.ExecRequest{
				Cmd:            []string{"sh", "-c", "touch /tmp/running; sleep 3"},
				TimeoutSeconds: 10,
			})
			first <- err
		}()

		deadline := time.Now().Add(2 * time.Second)
		for {
			res, err := vm.Exec(context.Background(), runtime.ExecRequest{Cmd: []string{"test", "-e", "/tmp/running"}})
			if err == nil && res.ExitCode == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("a second exec did not run while the first was in flight (last err %v)", err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := <-first; err != nil {
			t.Fatalf("first exec: %v", err)
		}
	})
}

func startVM(t *testing.T, rt *runtime.Firecracker, kernel, rootfs, id string) *runtime.VM {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	vm, err := rt.Start(ctx, runtime.Spec{
		ID:             id,
		KernelPath:     kernel,
		RootfsPath:     rootfs,
		RootfsReadOnly: true,
		VCPUs:          2,
		MemoryMiB:      512,
		VsockCID:       3,
		VsockPort:      guestproto.Port,
	})
	if err != nil {
		t.Fatalf("start %s: %v", id, err)
	}
	t.Cleanup(func() { stopVM(t, rt, vm) })
	return vm
}

func stopVM(t *testing.T, rt *runtime.Firecracker, vm *runtime.VM) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rt.Stop(ctx, vm); err != nil {
		t.Errorf("stop %s: %v", vm.ID, err)
	}
}

func doExec(t *testing.T, vm *runtime.VM, req runtime.ExecRequest) runtime.ExecResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := vm.Exec(ctx, req)
	if err != nil {
		t.Fatalf("exec %v: %v", req.Cmd, err)
	}
	return res
}

func runPreflight(t *testing.T, repo string) {
	t.Helper()
	cmd := exec.Command("bash", "scripts/preflight.sh")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("preflight failed: %v\n%s", err, out)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func kilnRoot() string {
	if root := os.Getenv("KILN_ROOT"); root != "" {
		return root
	}
	return runtime.ConfigRoot
}

// kernelPath returns the pinned kernel under the Kiln root.
func kernelPath(t *testing.T, repo, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, "scripts", "versions.env"))
	if err != nil {
		t.Fatal(err)
	}
	version := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "KERNEL_VERSION="); ok {
			version = strings.TrimSpace(v)
		}
	}
	if version == "" {
		t.Fatal("scripts/versions.env does not set KERNEL_VERSION")
	}
	path := filepath.Join(root, "kernel", "vmlinux-"+version)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("kernel %s: %v (run `kiln init`)", path, err)
	}
	return path
}
