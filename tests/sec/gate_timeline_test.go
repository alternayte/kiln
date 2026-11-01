//go:build kvm

package secgate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/snapshot"
	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

const (
	timelineTemplate = "sec-timeline"
	plainTemplate    = "sec-timeline-plain"
	timelineAllowed  = "example.com"
	timelineNarrowed = "example.org"
	// The counter lives in process memory and shows through a file.
	timelineCounter = `import os, time
n = 0
while True:
    n += 1
    open("/tmp/tick.tmp", "w").write(str(os.getpid()) + " " + str(n))
    os.rename("/tmp/tick.tmp", "/tmp/tick")
    time.sleep(0.05)
`
)

func TestSecTimeline(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("gate sec needs root: start it with sudo")
	}
	repo := repoRoot(t)
	root := kilnRoot()
	kernel := kernelPath(t, repo, root)
	kilnBin := buildKiln(t, repo)
	ctx, cancel := context.WithTimeout(context.Background(), gateTimeout)
	defer cancel()
	st, err := store.Open(filepath.Join(root, "kiln.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rt, err := runtime.New(runtime.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	nm := network.New(kilnBin, root)
	if err := nm.EnsureBase(ctx); err != nil {
		t.Fatal(err)
	}
	builder := template.Builder{KilninitPath: filepath.Join(root, "bin", "kilninit")}
	tmgr := &template.Manager{Root: root, Store: st, Runtime: rt, Network: nm, Builder: builder, KernelPath: kernel}
	sbx := sandbox.New(sandbox.Config{
		Root: root, Store: st, Runtime: rt, Network: nm, Zone: "timeline.test",
		Pages:      func() snapshot.PageFaultSource { return &snapshot.Process{Binary: kilnBin} },
		KernelPath: kernel,
	})
	destroyAll := func() {
		clean := context.WithoutCancel(ctx)
		rows, _ := st.ListSandboxes(clean)
		for _, row := range rows {
			if (row.TemplateName == timelineTemplate || row.TemplateName == plainTemplate) && row.DestroyedAt == nil {
				_ = sbx.Destroy(clean, row.ID)
			}
		}
		_ = tmgr.Delete(clean, timelineTemplate)
		_ = tmgr.Delete(clean, plainTemplate)
	}
	destroyAll()
	t.Cleanup(destroyAll)

	build(t, ctx, tmgr, template.BuildRequest{
		Name: timelineTemplate, Image: agentImage, VCPUs: 1, MemoryMB: 512, DiskMB: 2048,
		EgressAllow: []string{timelineAllowed, timelineNarrowed}, Timeline: true,
	}, store.TemplateReady)
	sb := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
		Template: timelineTemplate, Lifecycle: store.LifecyclePersistent, IdleSeconds: 900,
	})
	shell := func(t *testing.T, id, script string) string {
		t.Helper()
		res, err := sbx.Exec(ctx, id, runtime.ExecRequest{Cmd: []string{"sh", "-c", script}, TimeoutSeconds: 60}, nil)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("%s: %+v %v", script, res, err)
		}
		return strings.TrimSpace(res.Stdout)
	}
	tick := func(t *testing.T, id string) (string, int) {
		t.Helper()
		pid, count, _ := strings.Cut(shell(t, id, "cat /tmp/tick"), " ")
		n, err := strconv.Atoi(count)
		if err != nil {
			t.Fatalf("tick %q: %v", count, err)
		}
		return pid, n
	}
	mark := func(t *testing.T, label string) store.TimelinePoint {
		t.Helper()
		point, err := sbx.MarkPoint(ctx, sb.ID, label)
		if err != nil {
			t.Fatalf("mark %s: %v", label, err)
		}
		return point
	}
	if _, err := sbx.StartProcess(ctx, sb.ID, runtime.ProcessRequest{Cmd: []string{"python", "-c", timelineCounter}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for shellOK := false; !shellOK; {
		res, err := sbx.Exec(ctx, sb.ID, runtime.ExecRequest{Cmd: []string{"test", "-e", "/tmp/tick"}, TimeoutSeconds: 10}, nil)
		shellOK = err == nil && res.ExitCode == 0
		if !shellOK && time.Now().After(deadline) {
			t.Fatal("the counter never started")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := sbx.Publish(ctx, sb.ID, 8080, store.VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	shell(t, sb.ID, "echo first > /root/file; sync")
	first := mark(t, "first")
	if first.Kind != store.PointFull {
		t.Fatalf("the first point is %s", first.Kind)
	}
	firstPID, firstCount := tick(t, sb.ID)

	var second store.TimelinePoint
	t.Run("SecondPointIsADiff", func(t *testing.T) {
		shell(t, sb.ID, "echo second > /root/file; sync")
		second = mark(t, "second")
		if second.Kind != store.PointDiff {
			t.Errorf("the second point is %s", second.Kind)
		}
		if second.SizeBytes >= 512<<20 {
			t.Errorf("the second point holds %d bytes of a 512 MiB guest", second.SizeBytes)
		}
		t.Logf("first %d bytes, second %d bytes", first.SizeBytes, second.SizeBytes)
	})
	t.Run("RewindReturnsFileAndProcess", func(t *testing.T) {
		shell(t, sb.ID, "echo third > /root/file; sync")
		time.Sleep(3 * time.Second)
		_, before := tick(t, sb.ID)
		if before <= firstCount {
			t.Fatalf("the counter stands at %d after %d", before, firstCount)
		}
		rewound, err := sbx.Rewind(ctx, sb.ID, second.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rewound.ID != sb.ID || rewound.State != store.SandboxRunning {
			t.Errorf("after the rewind: %+v", rewound)
		}
		if got := shell(t, sb.ID, "cat /root/file"); got != "second" {
			t.Errorf("the file holds %q", got)
		}
		pid, after := tick(t, sb.ID)
		if pid != firstPID {
			t.Errorf("the process is %s, want %s", pid, firstPID)
		}
		if after >= before {
			t.Errorf("the counter stands at %d after a rewind from %d", after, before)
		}
		published, err := sbx.Published(ctx, sb.ID)
		if err != nil || len(published) != 1 {
			t.Errorf("the hostname after a rewind: %+v %v", published, err)
		}
	})
	t.Run("RewindRemovesLaterPoints", func(t *testing.T) {
		third := mark(t, "third")
		if third.Kind != store.PointDiff {
			t.Errorf("a point after a rewind is %s", third.Kind)
		}
		if _, err := sbx.Rewind(ctx, sb.ID, first.ID); err != nil {
			t.Fatal(err)
		}
		points, err := sbx.Points(ctx, sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(points) != 1 || points[0].ID != first.ID {
			t.Fatalf("after a rewind to the first point: %+v", points)
		}
		if got := shell(t, sb.ID, "cat /root/file"); got != "first" {
			t.Errorf("the file holds %q", got)
		}
		if _, err := os.Stat(filepath.Join(runtime.SandboxDir(root, sb.ID), "timeline", third.ID)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the later point left files: %v", err)
		}
	})
	t.Run("ForkFromAPoint", func(t *testing.T) {
		shell(t, sb.ID, "echo forked > /root/file; sync")
		point := mark(t, "fork")
		shell(t, sb.ID, "echo after > /root/file; sync")
		copies, err := sbx.ForkPoint(ctx, sb.ID, point.ID, 3, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, c := range copies {
			if got := shell(t, c.ID, "cat /root/file"); got != "forked" {
				t.Errorf("copy %s holds %q", c.ID, got)
			}
			seen[shell(t, c.ID, "head -c 16 /dev/urandom | od -An -tx1")] = true
		}
		if len(seen) != 3 {
			t.Errorf("three copies drew %d values of entropy", len(seen))
		}
		if got := shell(t, sb.ID, "cat /root/file"); got != "after" {
			t.Errorf("the origin holds %q", got)
		}
		for _, c := range copies {
			if err := sbx.Destroy(ctx, c.ID); err != nil {
				t.Error(err)
			}
		}
	})
	t.Run("DeleteAMiddlePoint", func(t *testing.T) {
		shell(t, sb.ID, "echo a > /root/a; sync")
		a := mark(t, "a")
		shell(t, sb.ID, "echo b > /root/b; sync")
		b := mark(t, "b")
		shell(t, sb.ID, "echo c > /root/c; sync")
		c := mark(t, "c")
		if err := sbx.DeletePoint(ctx, sb.ID, b.ID); err != nil {
			t.Fatal(err)
		}
		shell(t, sb.ID, "rm /root/a /root/b /root/c; sync")
		if _, err := sbx.Rewind(ctx, sb.ID, c.ID); err != nil {
			t.Fatal(err)
		}
		if got := shell(t, sb.ID, "cat /root/a /root/b /root/c | tr -d '\\n'"); got != "abc" {
			t.Errorf("the point after the delete holds %q", got)
		}
		if _, err := sbx.Rewind(ctx, sb.ID, a.ID); err != nil {
			t.Fatal(err)
		}
		if got := shell(t, sb.ID, "cat /root/a; test ! -e /root/b && echo gone"); got != "a\ngone" {
			t.Errorf("the point before the delete holds %q", got)
		}
	})
	t.Run("SleepKeepsPointsAndTheNextPointIsFull", func(t *testing.T) {
		before, err := sbx.Points(ctx, sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := sbx.Sleep(ctx, sb.ID); err != nil {
			t.Fatal(err)
		}
		after, err := sbx.Points(ctx, sb.ID)
		if err != nil || len(after) != len(before) {
			t.Fatalf("a sleep changed the points: %d to %d, %v", len(before), len(after), err)
		}
		if _, err := sbx.Wake(ctx, sb.ID); err != nil {
			t.Fatal(err)
		}
		// The woken VM counts its dirty pages from the sleep image.
		if point := mark(t, "woken"); point.Kind != store.PointFull {
			t.Errorf("a point after a wake is %s", point.Kind)
		}
	})
	t.Run("EgressStaysNarrowed", func(t *testing.T) {
		point := mark(t, "wide")
		if _, err := sbx.SetEgress(ctx, sb.ID, []string{timelineAllowed}); err != nil {
			t.Fatal(err)
		}
		rewound, err := sbx.Rewind(ctx, sb.ID, point.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rewound.EgressAllow) != 1 || rewound.EgressAllow[0] != timelineAllowed {
			t.Errorf("the row holds %v", rewound.EgressAllow)
		}
		lookup := fmt.Sprintf(`python -c "import socket; socket.gethostbyname('%s')"`, timelineNarrowed)
		res, err := sbx.Exec(ctx, sb.ID, runtime.ExecRequest{Cmd: []string{"sh", "-c", lookup}, TimeoutSeconds: 30}, nil)
		if err != nil || res.ExitCode == 0 {
			t.Errorf("a name the narrowed list leaves out resolved: %+v %v", res, err)
		}
	})
	t.Run("ThirtyThirdPointMergesTheOldest", func(t *testing.T) {
		points, err := sbx.Points(ctx, sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		var newest store.TimelinePoint
		for i := len(points); i < 33; i++ {
			shell(t, sb.ID, fmt.Sprintf("echo %d > /root/n; sync", i))
			newest = mark(t, fmt.Sprintf("n%d", i))
		}
		points, err = sbx.Points(ctx, sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(points) != 32 {
			t.Errorf("the chain holds %d points", len(points))
		}
		if points[0].Merged == 0 {
			t.Errorf("the list shows no merge: %+v", points[0])
		}
		if _, err := sbx.Rewind(ctx, sb.ID, newest.ID); err != nil {
			t.Fatal(err)
		}
		if got := shell(t, sb.ID, "cat /root/n"); got != "32" {
			t.Errorf("the newest point holds %q", got)
		}
	})
	t.Run("PlainTemplateAnswersConflict", func(t *testing.T) {
		build(t, ctx, tmgr, template.BuildRequest{
			Name: plainTemplate, Image: agentImage, VCPUs: 1, MemoryMB: 512, DiskMB: 2048, EgressAllow: []string{},
		}, store.TemplateReady)
		plain := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
			Template: plainTemplate, Lifecycle: store.LifecycleEphemeral, IdleSeconds: 900,
		})
		defer sbx.Destroy(context.WithoutCancel(ctx), plain.ID)
		if _, err := sbx.MarkPoint(ctx, plain.ID, ""); !errors.Is(err, store.ErrConflict) {
			t.Errorf("a template with no timeline answered %v", err)
		}
	})
	t.Run("ObserveAnswersConflict", func(t *testing.T) {
		if _, err := sbx.ObserveEgress(ctx, sb.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := sbx.MarkPoint(ctx, sb.ID, ""); !errors.Is(err, store.ErrConflict) {
			t.Errorf("a sandbox that observes answered %v", err)
		}
		if _, err := sbx.SetEgress(ctx, sb.ID, []string{timelineAllowed}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("DestroyLeavesOnlyWhatAForkReads", func(t *testing.T) {
		points, err := sbx.Points(ctx, sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		copies, err := sbx.ForkPoint(ctx, sb.ID, points[len(points)-1].ID, 1, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := sbx.Destroy(ctx, sb.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(runtime.SandboxDir(root, sb.ID), "timeline")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the destroy left point files: %v", err)
		}
		if rows, err := st.ListTimelinePoints(ctx, sb.ID); err != nil || len(rows) != 0 {
			t.Errorf("the destroy left point rows: %+v %v", rows, err)
		}
		if got := shell(t, copies[0].ID, "cat /root/n"); got == "" {
			t.Error("the fork lost its state")
		}
		child, err := st.GetSandbox(ctx, copies[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, "snapshots", child.SnapshotID)
		if _, err := os.Stat(filepath.Join(dir, "mem")); err != nil {
			t.Errorf("the fork's image went with its origin: %v", err)
		}
		if err := sbx.Destroy(ctx, child.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the last fork left its image: %v", err)
		}
	})
}
