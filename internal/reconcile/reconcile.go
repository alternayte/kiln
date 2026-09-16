// Package reconcile compares what the store intends with what the host has.
// The host is the truth: a row never proves that a process exists. A sweep
// adopts the microVMs that survived a daemon restart and destroys every host
// resource that no live row owns.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/snapshot"
	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

// sweepInterval is how often the reconciler compares intent with reality.
const sweepInterval = 60 * time.Second

// Reconciler sweeps the host. The store records intent; the host records
// reality.
type Reconciler struct {
	Root      string
	Store     store.Store
	Sandboxes *sandbox.Manager
	Network   *network.Manager
	Templates *template.Manager
	Runtime   *runtime.Firecracker
	// Interval overrides the sweep period. Tests set it.
	Interval time.Duration
	// Now returns the current time. Tests set it.
	Now func() time.Time
}

// Report is what one sweep changed.
type Report struct {
	Adopted []string
	Failed  []string
	Swept   []string
	Stuck   []string
}

// Run sweeps every Interval until the context ends. The caller runs one
// sweep first, so the listener never serves before the host is reconciled.
func (r *Reconciler) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = sweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := r.Sweep(ctx); err != nil {
				log.Printf("reconcile: sweep: %v", err)
			}
		}
	}
}

// Sweep compares the store with the host once.
func (r *Reconciler) Sweep(ctx context.Context) (Report, error) {
	if r.Sandboxes == nil || r.Runtime == nil || r.Network == nil {
		return Report{}, errors.New("reconcile: sandbox manager, runtime and network are required")
	}
	rows, err := r.Store.ListSandboxes(ctx)
	if err != nil {
		return Report{}, err
	}
	host := r.scan(ctx)
	protected := map[string]bool{}
	if r.Templates != nil {
		for id := range r.Templates.Protected() {
			protected[id] = true
		}
	}

	var report Report
	keep := map[string]store.Sandbox{}
	for _, row := range rows {
		if row.DestroyedAt != nil {
			continue
		}
		switch row.State {
		case store.SandboxRunning:
			if r.Sandboxes.Has(row.ID) {
				if _, alive := host.firecracker[row.ID]; alive {
					keep[row.ID] = row
					continue
				}
				r.failRow(ctx, row, "firecracker process is gone", host, &report)
				continue
			}
			if err := r.adopt(ctx, row, host, &report); err != nil {
				r.failRow(ctx, row, err.Error(), host, &report)
				continue
			}
			keep[row.ID] = row
		case store.SandboxCreating, store.SandboxWaking, store.SandboxStopping:
			// A request may still be working on this row. Only a state a
			// previous daemon left behind is stale.
			if r.Sandboxes.Active(row.ID) {
				keep[row.ID] = row
				continue
			}
			r.failRow(ctx, row, "interrupted while "+row.State, host, &report)
		case store.SandboxSleeping:
			if _, err := os.Stat(filepath.Join(runtime.SandboxDir(r.Root, row.ID), "sleep", "mem")); err != nil {
				r.failRow(ctx, row, "sleep image is missing", host, &report)
				continue
			}
			keep[row.ID] = row
		case store.SandboxFailed:
			// A failed row owns no host resource. The orphan pass removes
			// anything named after it.
		}
	}

	r.failStuckTemplates(ctx, &report)
	r.sweepOrphans(ctx, rows, keep, protected, host, &report)
	r.sweepRouting(ctx, &report)
	if err := r.Sandboxes.ScheduleAll(ctx); err != nil {
		return report, err
	}
	return report, nil
}

// adopt reattaches to a microVM that outlived the previous daemon.
func (r *Reconciler) adopt(ctx context.Context, row store.Sandbox, host *hostState, report *Report) error {
	pid, ok := host.firecracker[row.ID]
	if !ok {
		return fmt.Errorf("no firecracker process")
	}
	vm, err := r.Runtime.Adopt(ctx, runtime.AdoptSpec{ID: row.ID, PID: pid})
	if err != nil {
		return err
	}
	pagesPID, ok := host.snapfault[row.ID]
	if !ok {
		return fmt.Errorf("no page fault source")
	}
	var att *network.Attachment
	if row.TapName != "" {
		tpl, err := r.Store.GetTemplate(ctx, row.TemplateName)
		if err != nil {
			return err
		}
		att, err = r.Network.Adopt(ctx, row.ID, row.TapName, tpl.EgressAllow)
		if err != nil {
			return err
		}
	}
	r.Sandboxes.Adopt(ctx, row, vm, snapshot.Adopt(pagesPID), att)
	report.Adopted = append(report.Adopted, row.ID)
	return nil
}

// failRow records that a row cannot run and removes its host resources.
func (r *Reconciler) failRow(ctx context.Context, row store.Sandbox, reason string, host *hostState, report *Report) {
	saveCtx := context.WithoutCancel(ctx)
	r.Sandboxes.Forget(row.ID)
	if err := r.Store.SetSandboxState(saveCtx, row.ID, store.SandboxFailed); err != nil {
		log.Printf("reconcile: %s: record failure: %v", row.ID, err)
	}
	if err := r.Store.AppendEvent(saveCtx, store.Event{
		SandboxID: row.ID,
		FromState: row.State,
		ToState:   store.SandboxFailed,
		Reason:    reason,
		At:        r.now(),
	}); err != nil {
		log.Printf("reconcile: %s: failure event: %v", row.ID, err)
	}
	r.destroyHost(saveCtx, row.ID, row.TapName, host)
	report.Failed = append(report.Failed, row.ID)
}

// failStuckTemplates marks a template a dead daemon left building as failed.
func (r *Reconciler) failStuckTemplates(ctx context.Context, report *Report) {
	rows, err := r.Store.ListTemplates(ctx)
	if err != nil {
		log.Printf("reconcile: list templates: %v", err)
		return
	}
	for _, row := range rows {
		if row.State != store.TemplateBuilding {
			continue
		}
		if r.Templates != nil && r.Templates.IsBuilding(row.TenantID+"/"+row.Name) {
			continue
		}
		if err := r.Store.SetTemplateState(ctx, row.Name, store.TemplateFailed, "interrupted by restart"); err != nil {
			log.Printf("reconcile: template %s: record failure: %v", row.Name, err)
		}
		if err := r.Store.AppendEvent(ctx, store.Event{
			FromState: store.TemplateBuilding,
			ToState:   store.TemplateFailed,
			Reason:    "interrupted by restart",
			At:        r.now(),
		}); err != nil {
			log.Printf("reconcile: template %s: failure event: %v", row.Name, err)
		}
		if r.Templates != nil {
			_ = os.RemoveAll(r.Templates.TemplateDir(row.TenantID, row.Name))
		}
		report.Stuck = append(report.Stuck, row.Name)
	}
}

// sweepOrphans destroys every host resource that no live row owns. A build VM
// this process runs is not an orphan.
func (r *Reconciler) sweepOrphans(ctx context.Context, rows []store.Sandbox, keep map[string]store.Sandbox, protected map[string]bool, host *hostState, report *Report) {
	keptTaps := map[string]bool{}
	keptShorts := map[string]bool{}
	tapNames := map[string]string{}
	for _, row := range rows {
		if row.TapName != "" {
			tapNames[row.ID] = row.TapName
		}
	}
	for id, row := range keep {
		if row.TapName != "" {
			keptTaps[row.TapName] = true
		}
		keptShorts[network.ShortID(id)] = true
	}
	for id := range protected {
		keptShorts[network.ShortID(id)] = true
	}

	// Processes, directories and cgroups named after an id no row keeps.
	ids := map[string]bool{}
	for id := range host.firecracker {
		ids[id] = true
	}
	for id := range host.children {
		ids[id] = true
	}
	for id := range host.snapfault {
		ids[id] = true
	}
	for id := range host.dirs {
		ids[id] = true
	}
	for id := range host.cgroups {
		ids[id] = true
	}
	for _, row := range rows {
		if row.DestroyedAt == nil && row.State == store.SandboxFailed {
			ids[row.ID] = true
		}
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	for _, id := range ordered {
		if _, ok := keep[id]; ok {
			continue
		}
		if protected[id] {
			continue
		}
		report.Swept = append(report.Swept, id)
		r.destroyHost(ctx, id, tapNames[id], host)
	}

	// TAP devices without a row.
	for _, tap := range host.taps {
		if keptTaps[tap] {
			continue
		}
		if short, ok := strings.CutPrefix(tap, "kiln-"); ok && keptShorts[short] {
			continue
		}
		report.Swept = append(report.Swept, tap)
		if err := network.Purge(ctx, tap); err != nil {
			log.Printf("reconcile: purge %s: %v", tap, err)
		}
	}

	// Chains and their allow sets without a row.
	for _, short := range host.chains {
		if keptShorts[short] {
			continue
		}
		report.Swept = append(report.Swept, "kiln_"+short)
		if err := network.ForgetShort(ctx, short); err != nil {
			log.Printf("reconcile: forget chains %s: %v", short, err)
		}
	}
}

// destroyHost stops every host resource named after one sandbox id. It does
// not touch the store, so it works after a failure and during a sweep.
func (r *Reconciler) destroyHost(ctx context.Context, id, tapName string, host *hostState) {
	if host != nil {
		for _, pid := range host.children[id] {
			if err := runtime.KillProcess(pid); err != nil {
				log.Printf("reconcile: %s: kill %d: %v", id, pid, err)
			}
		}
		if pid := host.snapfault[id]; pid > 0 {
			if err := runtime.KillProcess(pid); err != nil {
				log.Printf("reconcile: %s: kill snapfault: %v", id, err)
			}
		}
	}
	if tapName == "" {
		tapName = "kiln-" + network.ShortID(id)
	}
	if _, err := exec.LookPath("ip"); err == nil {
		if _, err := os.Stat("/sys/class/net/" + tapName); err == nil {
			if err := network.Purge(ctx, tapName); err != nil {
				log.Printf("reconcile: %s: purge %s: %v", id, tapName, err)
			}
		}
	}
	if err := network.Forget(ctx, id); err != nil {
		log.Printf("reconcile: %s: forget chains: %v", id, err)
	}
	if err := runtime.RemoveCgroup(id); err != nil {
		log.Printf("reconcile: %s: remove cgroup: %v", id, err)
	}
	if err := os.RemoveAll(runtime.SandboxDir(r.Root, id)); err != nil {
		log.Printf("reconcile: %s: remove directory: %v", id, err)
	}
}

// hostState is what the kernel has now.
type hostState struct {
	firecracker map[string]int
	snapfault   map[string]int
	// children lists every process in a kiln/<id> cgroup. Jailer appears here
	// too, between its fork and the firecracker exec.
	children map[string][]int
	taps     []string
	chains   []string
	dirs     map[string]bool
	cgroups  map[string]bool
}

// scan reads the host. A command that fails leaves its section empty: the
// rows still reconcile, and the missing section is reported by the log.
func (r *Reconciler) scan(ctx context.Context) *hostState {
	h := &hostState{
		firecracker: map[string]int{},
		snapfault:   map[string]int{},
		children:    map[string][]int{},
		dirs:        map[string]bool{},
		cgroups:     map[string]bool{},
	}
	entries, err := os.ReadDir("/proc")
	if err == nil {
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid <= 0 {
				continue
			}
			if id, ok := cgroupSandboxID(pid); ok {
				h.children[id] = append(h.children[id], pid)
				if commandName(pid) == "firecracker" {
					h.firecracker[id] = pid
				}
				continue
			}
			if sock, ok := snapfaultSocket(pid); ok {
				if id, ok := sandboxIDFromPath(r.Root, sock); ok {
					h.snapfault[id] = pid
				}
			}
		}
	}
	if out, err := commandOutput(ctx, "ip", "-o", "link", "show"); err == nil {
		h.taps = parseTaps(out)
	}
	if out, err := commandOutput(ctx, "nft", "list", "table", "inet", "kiln"); err == nil {
		h.chains = parseChains(out)
	}
	if entries, err := os.ReadDir(filepath.Join(r.Root, "sandboxes")); err == nil {
		for _, entry := range entries {
			h.dirs[entry.Name()] = true
		}
	}
	if entries, err := os.ReadDir(runtime.ManagerCgroupDir()); err == nil {
		// A cgroup directory also holds interface files such as cgroup.procs.
		// Only child directories are sandbox cgroups.
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			h.cgroups[entry.Name()] = true
		}
	}
	return h
}

// cgroupSandboxID returns the sandbox id a process cgroup names. Jailer puts
// each microVM under /kiln/<id>.
func cgroupSandboxID(pid int) (string, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", false
	}
	return cgroupIDFromData(string(b))
}

// cgroupIDFromData reads the id out of a /proc/<pid>/cgroup file.
func cgroupIDFromData(data string) (string, bool) {
	for _, line := range strings.Split(data, "\n") {
		rest, ok := strings.CutPrefix(line, "0::/")
		if !ok {
			continue
		}
		rest, ok = strings.CutPrefix(rest, "kiln/")
		if !ok {
			continue
		}
		id, _, _ := strings.Cut(rest, "/")
		if id != "" {
			return id, true
		}
	}
	return "", false
}

// commandName returns the short name of one process.
func commandName(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// snapfaultSocket returns the --sock argument of a snapfault process.
func snapfaultSocket(pid int) (string, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", false
	}
	return snapfaultSocketFromCmdline(string(b))
}

// snapfaultSocketFromCmdline reads the --sock argument out of a cmdline.
func snapfaultSocketFromCmdline(cmdline string) (string, bool) {
	args := strings.Split(strings.TrimSuffix(cmdline, "\x00"), "\x00")
	found := false
	for _, arg := range args {
		if arg == "snapfault" {
			found = true
			break
		}
	}
	if !found {
		return "", false
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--sock" {
			return args[i+1], true
		}
	}
	return "", false
}

// sandboxIDFromPath returns the sandbox id inside a path under the Kiln root.
func sandboxIDFromPath(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "sandboxes" && parts[i+1] != "" {
			return parts[i+1], true
		}
	}
	return "", false
}

// parseTaps reads the kiln- interface names from ip -o link show.
func parseTaps(out string) []string {
	var taps []string
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSpace(parts[1])
		if strings.HasPrefix(name, "kiln-") {
			taps = append(taps, name)
		}
	}
	return taps
}

// parseChains reads the short ids of the kiln_ chains from nft output.
func parseChains(out string) []string {
	var shorts []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "chain" {
			continue
		}
		rest, ok := strings.CutPrefix(fields[1], "kiln_")
		if !ok {
			continue
		}
		i := strings.LastIndex(rest, "_")
		if i <= 0 {
			continue
		}
		shorts = append(shorts, rest[:i])
	}
	return shorts
}

// sweepRouting removes a fwmark rule whose routing table points at a device
// that no longer exists. A killed daemon can leave those behind.
func (r *Reconciler) sweepRouting(ctx context.Context, report *Report) {
	out, err := commandOutput(ctx, "ip", "rule", "show")
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		mark, table, ok := network.RuleFor(line)
		if !ok {
			continue
		}
		route, err := commandOutput(ctx, "ip", "route", "show", "table", strconv.Itoa(table))
		if err != nil {
			continue
		}
		dev := routeDevice(route)
		if dev == "" {
			continue
		}
		if _, err := os.Stat("/sys/class/net/" + dev); err == nil {
			continue
		}
		report.Swept = append(report.Swept, "rule "+strconv.Itoa(mark))
		_, _ = exec.CommandContext(ctx, "ip", "route", "flush", "table", strconv.Itoa(table)).CombinedOutput()
		_, _ = exec.CommandContext(ctx, "ip", "rule", "del", "fwmark", strconv.Itoa(mark), "to", runtime.GuestIP, "lookup", strconv.Itoa(table)).CombinedOutput()
	}
}

// routeDevice returns the device a route line sends traffic through.
func routeDevice(route string) string {
	fields := strings.Fields(route)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "dev" {
			return fields[i+1]
		}
	}
	return ""
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}
