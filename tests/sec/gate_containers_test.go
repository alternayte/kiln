//go:build kvm

package secgate

import (
	"context"
	"strings"
	"testing"

	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/store"
)

// checkContainerSupport holds what rootless containers need from the guest
// (issues 22 and 23): a cgroup2 mount, a cgroup subtree the template user
// owns, and a data directory on the overlay drive that overlayfs stacks on.
// Each holds in the sandbox, in a fork and in a restore of a snapshot, and two
// copies never see each other's data.
func checkContainerSupport(t *testing.T, ctx context.Context, sbx *sandbox.Manager, sb store.Sandbox) {
	if out := guestOutput(t, ctx, sbx, sb.ID, "sh", "-c", "echo origin > /var/lib/kiln-data/origin"); out != "" {
		t.Fatalf("the user cannot write the data directory: %q", out)
	}
	assertContainerGuest(t, ctx, sbx, sb.ID)
	assertImageModes(t, ctx, sbx, sb.ID)
	assertUserDevices(t, ctx, sbx, sb.ID)
	assertHosts(t, ctx, sbx, sb.ID)

	copies, err := sbx.Fork(ctx, sb.ID, 2, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, c := range copies {
			_ = sbx.Destroy(context.WithoutCancel(ctx), c.ID)
		}
	}()
	for _, c := range copies {
		assertContainerGuest(t, ctx, sbx, c.ID)
		assertHosts(t, ctx, sbx, c.ID)
		if out := guestOutput(t, ctx, sbx, c.ID, "cat", "/var/lib/kiln-data/origin"); strings.TrimSpace(out) != "origin" {
			t.Errorf("fork %s lost the data written before the fork: %q", c.ID, out)
		}
	}
	guestOutput(t, ctx, sbx, copies[0].ID, "sh", "-c", "echo first > /var/lib/kiln-data/first")
	for _, id := range []string{sb.ID, copies[1].ID} {
		if out := guestOutput(t, ctx, sbx, id, "ls", "/var/lib/kiln-data"); strings.Contains(out, "first") {
			t.Errorf("%s sees the data of another fork: %q", id, out)
		}
	}

	snap, err := sbx.Snapshot(ctx, copies[0].ID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sbx.DeleteSnapshot(context.WithoutCancel(ctx), snap.ID) }()
	restored, err := sbx.RestoreSnapshot(ctx, snap.ID, 1, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	copies = append(copies, restored...)
	assertContainerGuest(t, ctx, sbx, restored[0].ID)
	if out := guestOutput(t, ctx, sbx, restored[0].ID, "cat", "/var/lib/kiln-data/first"); strings.TrimSpace(out) != "first" {
		t.Errorf("the restore lost the data of its snapshot: %q", out)
	}
}

// assertContainerGuest checks one running guest.
func assertContainerGuest(t *testing.T, ctx context.Context, sbx *sandbox.Manager, id string) {
	t.Helper()
	if out := guestOutput(t, ctx, sbx, id, "stat", "-f", "-c", "%T", "/sys/fs/cgroup"); strings.TrimSpace(out) != "cgroup2fs" {
		t.Errorf("%s: /sys/fs/cgroup is %q", id, out)
	}
	if out := guestOutput(t, ctx, sbx, id, "cat", "/proc/self/cgroup"); strings.TrimSpace(out) != "0::/user/init" {
		t.Errorf("%s: exec runs in the cgroup %q", id, out)
	}
	// The user makes a cgroup of its own, limits its memory, and moves into
	// it. A write past the limit then kills the process.
	limit := `set -e
mkdir -p /sys/fs/cgroup/user/limited
echo 67108864 > /sys/fs/cgroup/user/limited/memory.max
echo 0 > /sys/fs/cgroup/user/limited/memory.swap.max 2>/dev/null || true
echo $$ > /sys/fs/cgroup/user/limited/cgroup.procs
cat /sys/fs/cgroup/user/limited/memory.max
python -c "b = b'x' * (256 << 20)" && echo survived`
	if out := guestOutput(t, ctx, sbx, id, "sh", "-c", limit); !strings.HasPrefix(out, "67108864\n") || strings.Contains(out, "survived") {
		t.Errorf("%s: a memory limit in the delegated cgroup gave %q", id, out)
	}
	if out := guestOutput(t, ctx, sbx, id, "stat", "-f", "-c", "%T", "/var/lib/kiln-data"); strings.TrimSpace(out) != "ext2/ext3" {
		t.Errorf("%s: /var/lib/kiln-data is %q", id, out)
	}
	if out := guestOutput(t, ctx, sbx, id, "stat", "-c", "%u", "/var/lib/kiln-data"); strings.TrimSpace(out) != agentUID {
		t.Errorf("%s: /var/lib/kiln-data belongs to %q", id, out)
	}
	// Rootless overlay2 stacks overlayfs on the data directory from inside a
	// user namespace.
	stack := `set -e
d=/var/lib/kiln-data/stack-$$
mkdir -p $d/lower $d/upper $d/work $d/merged
echo lower > $d/lower/f
unshare -Urm sh -c "mount -t overlay overlay -o lowerdir=$d/lower,upperdir=$d/upper,workdir=$d/work $d/merged && echo upper > $d/merged/f && cat $d/merged/f"
cat $d/upper/f`
	if out := guestOutput(t, ctx, sbx, id, "sh", "-c", stack); strings.TrimSpace(out) != "upper\nupper" {
		t.Errorf("%s: overlayfs on the data directory gave %q", id, out)
	}
}

// assertImageModes checks that the rootfs keeps the special bits of the image
// (issue 26): newuidmap and its kind need setuid for rootlesskit, and /tmp
// needs the sticky bit. chage reads /etc/shadow through its setgid bit, so it
// works for the template user only when the bit takes effect.
func assertImageModes(t *testing.T, ctx context.Context, sbx *sandbox.Manager, id string) {
	t.Helper()
	want := "4755 root:root /usr/bin/su\n4755 root:root /usr/bin/passwd\n2755 root:shadow /usr/bin/chage\n1777 root:root /tmp\n1777 root:root /var/tmp"
	if out := guestOutput(t, ctx, sbx, id, "stat", "-c", "%a %U:%G %n", "/usr/bin/su", "/usr/bin/passwd", "/usr/bin/chage", "/tmp", "/var/tmp"); strings.TrimSpace(out) != want {
		t.Errorf("%s: the image modes are\n%s\nwant\n%s", id, out, want)
	}
	if out := guestOutput(t, ctx, sbx, id, "chage", "-l", "nobody"); !strings.Contains(out, "Last password change") {
		t.Errorf("%s: a setgid binary did not take effect: %q", id, out)
	}
}

// tapScript makes a TAP device through /dev/net/tun, as slirp4netns does for
// rootless Docker.
const tapScript = `import fcntl, struct
f = open("/dev/net/tun", "r+b", buffering=0)
fcntl.ioctl(f, 0x400454ca, struct.pack("16sH", b"tap0", 0x1002))
print("ok")`

// assertUserDevices checks that the template user opens /dev/net/tun and
// /dev/fuse (issue 27), makes a TAP device in a network namespace it created,
// and still cannot make one in the namespace of the guest.
func assertUserDevices(t *testing.T, ctx context.Context, sbx *sandbox.Manager, id string) {
	t.Helper()
	modes := `stat -c %a /dev/net/tun; [ ! -e /dev/fuse ] || [ "$(stat -c %a /dev/fuse)" = 666 ] && echo fuse`
	if out := guestOutput(t, ctx, sbx, id, "sh", "-c", modes); strings.TrimSpace(out) != "666\nfuse" {
		t.Errorf("%s: the user devices are %q", id, out)
	}
	if out := guestOutput(t, ctx, sbx, id, "unshare", "-Urn", "python", "-c", tapScript); strings.TrimSpace(out) != "ok" {
		t.Errorf("%s: a TAP device in the user's own namespace gave %q", id, out)
	}
	if out := guestOutput(t, ctx, sbx, id, "python", "-c", tapScript); strings.TrimSpace(out) == "ok" {
		t.Errorf("%s: the user made a TAP device in the guest namespace: %q", id, out)
	}
}

// assertHosts checks that a guest whose image holds no /etc/hosts resolves
// localhost and its own sandbox id through files (issue 29). rootlesskit
// opens the file, and it fails when the file is missing.
func assertHosts(t *testing.T, ctx context.Context, sbx *sandbox.Manager, id string) {
	t.Helper()
	if out := guestOutput(t, ctx, sbx, id, "getent", "hosts", "localhost"); !strings.HasPrefix(out, "::1") && !strings.HasPrefix(out, "127.0.0.1") {
		t.Errorf("%s: localhost resolves to %q", id, out)
	}
	if out := guestOutput(t, ctx, sbx, id, "getent", "hosts", id); !strings.HasPrefix(out, "127.0.1.1") {
		t.Errorf("%s: its hostname resolves to %q", id, out)
	}
}
