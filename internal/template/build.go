package template

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/alternayte/kiln/internal/guestproto"
	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/store"
)

// setupTimeoutSeconds caps one setup command. A slow package install fits.
const setupTimeoutSeconds = guestproto.ExecTimeoutMax

// namePattern is the safe form of a template name. It becomes a directory
// name and part of a VM id.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// BuildRequest is one template build.
type BuildRequest struct {
	Name        string
	Image       string
	VCPUs       int
	MemoryMB    int
	DiskMB      int
	Setup       []string
	EgressAllow []string
}

// InvalidError reports a request that cannot be accepted.
type InvalidError struct {
	Message string
}

func (e *InvalidError) Error() string { return "invalid: " + e.Message }

// IsInvalid reports whether err is a rejected request.
func IsInvalid(err error) bool {
	var e *InvalidError
	return errors.As(err, &e)
}

func invalidf(format string, args ...any) error {
	return &InvalidError{Message: fmt.Sprintf(format, args...)}
}

// Validate rejects a request before any host resource is touched.
func (r BuildRequest) Validate() error {
	if !namePattern.MatchString(r.Name) {
		return invalidf("name %q must match %s", r.Name, namePattern)
	}
	if strings.TrimSpace(r.Image) == "" {
		return invalidf("image is required")
	}
	if r.VCPUs < 1 || r.VCPUs > 32 {
		return invalidf("vcpus %d is outside 1..32", r.VCPUs)
	}
	if r.MemoryMB < 1 || r.MemoryMB > 1<<20 {
		return invalidf("memory_mb %d is outside 1..%d", r.MemoryMB, 1<<20)
	}
	if r.DiskMB < 1 || r.DiskMB > 1<<20 {
		return invalidf("disk_mb %d is outside 1..%d", r.DiskMB, 1<<20)
	}
	for i, cmd := range r.Setup {
		if strings.TrimSpace(cmd) == "" {
			return invalidf("setup[%d] is empty", i)
		}
	}
	if err := network.ValidateAllow(r.EgressAllow); err != nil {
		return &InvalidError{Message: err.Error()}
	}
	return nil
}

// Info is a template row with what the host knows about its files.
type Info struct {
	store.Template
	SnapshotBytes int64
	Sandboxes     int
}

// Manager turns an OCI reference into a stored template snapshot. The build
// VM has an internal id and no sandbox row.
type Manager struct {
	Root    string
	Store   store.Store
	Runtime *runtime.Firecracker
	Network *network.Manager
	Builder Builder

	// KernelPath is the pinned vmlinux the build VM boots.
	KernelPath string
	// Now returns the current time. Tests set it.
	Now func() time.Time

	nextCID atomic.Uint32
}

// TemplateDir is the on-disk directory of one template.
func (m *Manager) TemplateDir(name string) string {
	return filepath.Join(m.Root, "templates", name)
}

// Start records the build and returns before it runs. A name that is building
// or ready is refused with store.ErrConflict.
func (m *Manager) Start(ctx context.Context, req BuildRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	now := m.now()
	row := store.Template{
		Name:        req.Name,
		ImageRef:    req.Image,
		VCPUs:       req.VCPUs,
		MemoryMB:    req.MemoryMB,
		DiskMB:      req.DiskMB,
		EgressAllow: req.EgressAllow,
		State:       store.TemplateBuilding,
		CreatedAt:   now,
	}
	if err := m.Store.StartTemplateBuild(ctx, row); err != nil {
		return err
	}
	if err := m.Store.AppendEvent(ctx, store.Event{
		FromState: "",
		ToState:   store.TemplateBuilding,
		Reason:    "build started",
		At:        now,
	}); err != nil {
		// The build itself matters more than its first event.
		log.Printf("template: %s: start event: %v", req.Name, err)
	}
	go m.build(ctx, req)
	return nil
}

// TemplateInfo returns the row with its snapshot size and child count.
func (m *Manager) TemplateInfo(ctx context.Context, name string) (Info, error) {
	row, err := m.Store.GetTemplate(ctx, name)
	if err != nil {
		return Info{}, err
	}
	info := Info{Template: row}
	for _, file := range []string{"mem", "state"} {
		if fi, err := os.Stat(filepath.Join(m.TemplateDir(name), file)); err == nil {
			info.SnapshotBytes += fi.Size()
		}
	}
	live, _, err := m.Store.TemplateDependents(ctx, name)
	if err != nil {
		return Info{}, err
	}
	info.Sandboxes = live
	return info, nil
}

// Delete removes a template and its files. A live sandbox or a snapshot row
// refuses the delete.
func (m *Manager) Delete(ctx context.Context, name string) error {
	row, err := m.Store.GetTemplate(ctx, name)
	if err != nil {
		return err
	}
	if row.State == store.TemplateBuilding {
		return store.ErrConflict
	}
	live, snapshots, err := m.Store.TemplateDependents(ctx, name)
	if err != nil {
		return err
	}
	if live > 0 || snapshots > 0 {
		return store.ErrConflict
	}
	if err := os.RemoveAll(m.TemplateDir(name)); err != nil {
		return err
	}
	return m.Store.DeleteTemplate(ctx, name)
}

// build runs one build and records its terminal state.
func (m *Manager) build(ctx context.Context, req BuildRequest) {
	err := m.runBuild(ctx, req)
	now := m.now()
	message := ""
	state := store.TemplateReady
	reason := "template ready"
	if err != nil {
		state = store.TemplateFailed
		message = err.Error()
		reason = "build failed"
		if rerr := os.RemoveAll(m.TemplateDir(req.Name)); rerr != nil {
			message = fmt.Sprintf("%s; remove template dir: %v", message, rerr)
		}
	}
	// The build must be recorded even when the server context is stopping.
	saveCtx := context.WithoutCancel(ctx)
	if serr := m.Store.SetTemplateState(saveCtx, req.Name, state, message); serr != nil {
		log.Printf("template: %s: record %s: %v", req.Name, state, serr)
	}
	if eerr := m.Store.AppendEvent(saveCtx, store.Event{
		FromState: store.TemplateBuilding,
		ToState:   state,
		Reason:    reason,
		At:        now,
	}); eerr != nil {
		log.Printf("template: %s: event: %v", req.Name, eerr)
	}
}

func (m *Manager) runBuild(ctx context.Context, req BuildRequest) (err error) {
	dir := m.TemplateDir(req.Name)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")
	imported, err := m.Builder.Import(ctx, ImportSpec{Ref: req.Image, RootfsPath: rootfs, SizeMB: req.DiskMB})
	if err != nil {
		return err
	}
	if err := m.Store.SetTemplateDigest(ctx, req.Name, imported.Digest); err != nil {
		return err
	}

	att, err := m.Network.Attach(ctx, m.buildID(req.Name), req.EgressAllow)
	if err != nil {
		return err
	}
	defer func() {
		if derr := att.Detach(context.WithoutCancel(ctx)); derr != nil {
			if err == nil {
				err = derr
			} else {
				err = fmt.Errorf("%w; detach: %v", err, derr)
			}
		}
	}()

	vm, err := m.Runtime.Start(ctx, runtime.Spec{
		ID:             m.buildID(req.Name),
		KernelPath:     m.KernelPath,
		RootfsPath:     rootfs,
		RootfsReadOnly: false,
		VCPUs:          req.VCPUs,
		MemoryMiB:      req.MemoryMB,
		VsockCID:       m.allocCID(),
		VsockPort:      guestproto.Port,
		TAPName:        att.TAPName,
	})
	if err != nil {
		return err
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		if serr := m.Runtime.Stop(stopCtx, vm); serr != nil {
			if err == nil {
				err = serr
			} else {
				err = fmt.Errorf("%w; stop: %v", err, serr)
			}
		}
	}()

	for i, cmd := range req.Setup {
		res, execErr := vm.Exec(ctx, runtime.ExecRequest{
			Cmd:            []string{"sh", "-c", cmd},
			TimeoutSeconds: setupTimeoutSeconds,
		})
		if execErr != nil {
			return fmt.Errorf("setup[%d] %q: %w", i, cmd, execErr)
		}
		if res.TimedOut || res.ExitCode != 0 {
			output := strings.TrimSpace(strings.Join([]string{res.Stderr, res.Stdout}, "\n"))
			return fmt.Errorf("setup[%d] %q: exit %d timed_out=%v: %s", i, cmd, res.ExitCode, res.TimedOut, output)
		}
	}

	if _, err := m.Runtime.Snapshot(ctx, vm, dir); err != nil {
		return err
	}
	return writeManifest(dir, Manifest{
		Name:        req.Name,
		ImageRef:    req.Image,
		ImageDigest: imported.Digest,
		VCPUs:       req.VCPUs,
		MemoryMB:    req.MemoryMB,
		DiskMB:      req.DiskMB,
		EgressAllow: req.EgressAllow,
		Setup:       req.Setup,
		CreatedAt:   m.now(),
	})
}

// buildID is the internal id of a build VM. It has no sandbox row, but its
// TAP, chains and directory carry the id so a sweep can find them.
func (m *Manager) buildID(name string) string { return "build-" + name }

// allocCID hands out distinct guest CIDs for concurrent builds.
func (m *Manager) allocCID() uint32 { return m.nextCID.Add(1) + 2 }

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now().UTC()
}
