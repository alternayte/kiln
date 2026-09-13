// Package sandbox owns the sandbox lifecycle: restore, exec, files, destroy.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/alternayte/kiln/internal/guestproto"
	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/snapshot"
	"github.com/alternayte/kiln/internal/store"
	"golang.org/x/sys/unix"
)

// ErrExhausted reports that the host has no room for another sandbox.
var ErrExhausted = errors.New("sandbox: exhausted")

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

// Config holds the sandbox manager dependencies.
type Config struct {
	Root    string
	Store   store.Store
	Runtime *runtime.Firecracker
	Network *network.Manager
	// Pages returns the page fault source for one restore.
	Pages func() snapshot.PageFaultSource
	// KernelPath is the pinned vmlinux the restored state references.
	KernelPath string
	// Secrets maps secret names to values. Values are never written to disk.
	Secrets map[string]string
	// Now returns the current time. Tests set it.
	Now func() time.Time
}

// Manager creates and destroys sandboxes.
type Manager struct {
	cfg Config

	mu    sync.Mutex
	vms   map[string]*runtime.VM
	pages map[string]snapshot.PageFaultSource
	attrs map[string]*network.Attachment
}

// New returns a sandbox manager.
func New(cfg Config) *Manager {
	return &Manager{
		cfg:   cfg,
		vms:   map[string]*runtime.VM{},
		pages: map[string]snapshot.PageFaultSource{},
		attrs: map[string]*network.Attachment{},
	}
}

// CreateRequest is one POST /v1/sandboxes body.
type CreateRequest struct {
	Template    string
	Lifecycle   string
	IdleSeconds int
	TTLSeconds  *int
	Metadata    map[string]any
	Secrets     []string
}

func (r CreateRequest) validate() error {
	if r.Template == "" {
		return invalidf("template is required")
	}
	switch r.Lifecycle {
	case store.LifecycleEphemeral, store.LifecyclePersistent:
	default:
		return invalidf("lifecycle must be %q or %q", store.LifecycleEphemeral, store.LifecyclePersistent)
	}
	if r.IdleSeconds <= 0 {
		return invalidf("idle_seconds must be greater than zero")
	}
	if r.TTLSeconds != nil && *r.TTLSeconds <= 0 {
		return invalidf("ttl_seconds must be greater than zero or null")
	}
	return nil
}

// Create restores a template snapshot into a running sandbox. It returns when
// the sandbox answers a fresh vsock hello and the resume hooks have run.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (store.Sandbox, error) {
	if err := req.validate(); err != nil {
		return store.Sandbox{}, err
	}
	tpl, err := m.cfg.Store.GetTemplate(ctx, req.Template)
	if err != nil {
		return store.Sandbox{}, err
	}
	if tpl.State != store.TemplateReady {
		return store.Sandbox{}, store.ErrConflict
	}
	env := make(map[string]string, len(req.Secrets))
	for _, name := range req.Secrets {
		value, ok := m.cfg.Secrets[name]
		if !ok {
			return store.Sandbox{}, invalidf("unknown secret %q", name)
		}
		env[name] = value
	}
	if err := checkSpace(m.cfg.Root, int64(tpl.DiskMB)); err != nil {
		return store.Sandbox{}, err
	}

	id, err := newID()
	if err != nil {
		return store.Sandbox{}, err
	}
	metadata := "{}"
	if len(req.Metadata) > 0 {
		b, err := json.Marshal(req.Metadata)
		if err != nil {
			return store.Sandbox{}, invalidf("metadata: %v", err)
		}
		metadata = string(b)
	}
	now := m.now()
	row := store.Sandbox{
		ID:           id,
		TemplateName: tpl.Name,
		Lifecycle:    req.Lifecycle,
		State:        store.SandboxCreating,
		TTLSeconds:   req.TTLSeconds,
		IdleSeconds:  req.IdleSeconds,
		LastActiveAt: now,
		Metadata:     metadata,
		CreatedAt:    now,
	}
	if err := m.cfg.Store.CreateSandbox(ctx, row); err != nil {
		return store.Sandbox{}, err
	}
	m.event(ctx, id, "", store.SandboxCreating, "create", now)

	running, err := m.restore(ctx, tpl, row, env)
	if err != nil {
		// The restore must not leave a VM, a TAP or files behind.
		m.cleanup(context.WithoutCancel(ctx), id)
		saveCtx := context.WithoutCancel(ctx)
		if serr := m.cfg.Store.SetSandboxState(saveCtx, id, store.SandboxFailed); serr != nil {
			log.Printf("sandbox: %s: record failure: %v", id, serr)
		}
		m.event(saveCtx, id, store.SandboxCreating, store.SandboxFailed, err.Error(), m.now())
		return store.Sandbox{}, err
	}
	return running, nil
}

func (m *Manager) restore(ctx context.Context, tpl store.Template, row store.Sandbox, env map[string]string) (store.Sandbox, error) {
	dir := runtime.SandboxDir(m.cfg.Root, row.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return store.Sandbox{}, err
	}
	overlay := filepath.Join(dir, "overlay.ext4")
	if err := createOverlay(ctx, overlay, int64(tpl.DiskMB)); err != nil {
		return store.Sandbox{}, err
	}
	att, err := m.cfg.Network.Attach(ctx, row.ID, nil)
	if err != nil {
		return store.Sandbox{}, err
	}
	m.mu.Lock()
	m.attrs[row.ID] = att
	m.mu.Unlock()
	if err := m.cfg.Store.SetSandboxRuntime(ctx, row.ID, att.TAPName, nil, 0); err != nil {
		return store.Sandbox{}, err
	}

	pages := m.cfg.Pages()
	templateDir := filepath.Join(m.cfg.Root, "templates", tpl.Name)
	sock := runtime.RestoreUffdSocket(m.cfg.Root, row.ID)
	if err := pages.Start(ctx, filepath.Join(templateDir, "mem"), sock); err != nil {
		return store.Sandbox{}, err
	}
	m.mu.Lock()
	m.pages[row.ID] = pages
	m.mu.Unlock()

	vm, err := m.cfg.Runtime.Restore(ctx, runtime.RestoreSpec{
		Spec: runtime.Spec{
			ID:          row.ID,
			KernelPath:  m.cfg.KernelPath,
			RootfsPath:  filepath.Join(templateDir, "rootfs.ext4"),
			OverlayPath: overlay,
			VCPUs:       tpl.VCPUs,
			MemoryMiB:   tpl.MemoryMB,
			VsockCID:    3,
			VsockPort:   guestproto.Port,
			TAPName:     att.TAPName,
		},
		StatePath:  filepath.Join(templateDir, "state"),
		UffdSocket: sock,
	})
	if err != nil {
		return store.Sandbox{}, err
	}
	m.mu.Lock()
	m.vms[row.ID] = vm
	m.mu.Unlock()

	entropy := make([]byte, 64)
	if _, err := rand.Read(entropy); err != nil {
		return store.Sandbox{}, err
	}
	if err := vm.ResumeHooks(ctx, entropy, time.Now().UnixNano(), row.ID, env); err != nil {
		return store.Sandbox{}, err
	}
	if err := m.cfg.Store.SetSandboxRuntime(ctx, row.ID, att.TAPName, nil, vm.PID); err != nil {
		return store.Sandbox{}, err
	}
	if err := m.cfg.Store.SetSandboxState(ctx, row.ID, store.SandboxRunning); err != nil {
		return store.Sandbox{}, err
	}
	row.State = store.SandboxRunning
	row.TapName = att.TAPName
	row.PID = vm.PID
	m.event(ctx, row.ID, store.SandboxCreating, store.SandboxRunning, "restored", m.now())
	return row, nil
}

// Destroy stops a sandbox and removes every host resource named after it. It
// is idempotent.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	row, err := m.cfg.Store.GetSandbox(ctx, id)
	if err != nil {
		return err
	}
	if row.State == store.SandboxDestroyed || row.DestroyedAt != nil {
		return nil
	}
	if err := m.cfg.Store.SetSandboxState(ctx, id, store.SandboxStopping); err != nil {
		return err
	}
	m.event(ctx, id, row.State, store.SandboxStopping, "destroy", m.now())
	m.cleanup(ctx, id)
	if err := os.RemoveAll(runtime.SandboxDir(m.cfg.Root, id)); err != nil {
		return err
	}
	now := m.now()
	if err := m.cfg.Store.DestroySandbox(ctx, id, now); err != nil {
		return err
	}
	m.event(ctx, id, store.SandboxStopping, store.SandboxDestroyed, "destroyed", now)
	return nil
}

// Get returns one sandbox row.
func (m *Manager) Get(ctx context.Context, id string) (store.Sandbox, error) {
	return m.cfg.Store.GetSandbox(ctx, id)
}

// List returns every sandbox row.
func (m *Manager) List(ctx context.Context) ([]store.Sandbox, error) {
	return m.cfg.Store.ListSandboxes(ctx)
}

// cleanup stops every host resource of one sandbox. It is best effort: each
// step runs even when an earlier one fails.
func (m *Manager) cleanup(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	m.mu.Lock()
	vm := m.vms[id]
	pages := m.pages[id]
	att := m.attrs[id]
	delete(m.vms, id)
	delete(m.pages, id)
	delete(m.attrs, id)
	m.mu.Unlock()

	if vm != nil {
		if err := m.cfg.Runtime.Stop(ctx, vm); err != nil {
			log.Printf("sandbox: %s: stop: %v", id, err)
		}
	}
	if pages != nil {
		if err := pages.Stop(); err != nil {
			log.Printf("sandbox: %s: snapfault: %v", id, err)
		}
	}
	if att != nil {
		if err := att.Detach(ctx); err != nil {
			log.Printf("sandbox: %s: network: %v", id, err)
		}
	}
}

// running returns the live VM of a running sandbox.
func (m *Manager) running(ctx context.Context, id string) (*runtime.VM, error) {
	row, err := m.cfg.Store.GetSandbox(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.State != store.SandboxRunning {
		return nil, store.ErrConflict
	}
	m.mu.Lock()
	vm := m.vms[id]
	m.mu.Unlock()
	if vm == nil {
		return nil, store.ErrConflict
	}
	return vm, nil
}

func (m *Manager) event(ctx context.Context, id, from, to, reason string, at time.Time) {
	if err := m.cfg.Store.AppendEvent(ctx, store.Event{
		SandboxID: id,
		FromState: from,
		ToState:   to,
		Reason:    reason,
		At:        at,
	}); err != nil {
		log.Printf("sandbox: %s: event: %v", id, err)
	}
}

func (m *Manager) now() time.Time {
	if m.cfg.Now != nil {
		return m.cfg.Now()
	}
	return time.Now().UTC()
}

// newID returns a short random sandbox id. It is safe as a directory name and
// as part of a TAP device name.
func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// createOverlay makes the sparse writable drive of one sandbox.
func createOverlay(ctx context.Context, path string, sizeMB int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := f.Truncate(sizeMB << 20); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "mke2fs", "-q", "-F", "-t", "ext4", "-L", "kiln-overlay", path).CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox: mke2fs: %w: %s", err, out)
	}
	return nil
}

// checkSpace refuses a create that cannot fit the writable drive.
func checkSpace(root string, needMB int64) error {
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return err
	}
	if int64(st.Bavail)*int64(st.Bsize) < needMB<<20 {
		return ErrExhausted
	}
	return nil
}

// Exec runs one command in a running sandbox.
func (m *Manager) Exec(ctx context.Context, id string, req runtime.ExecRequest, onOutput func(byte, []byte) error) (runtime.ExecResult, error) {
	vm, err := m.running(ctx, id)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	res, err := vm.ExecStream(ctx, req, onOutput)
	if err == nil {
		_ = m.cfg.Store.TouchSandbox(ctx, id, m.now())
	}
	return res, err
}

// OpenFile opens one guest file for reading.
func (m *Manager) OpenFile(ctx context.Context, id, path string) (io.ReadCloser, error) {
	vm, err := m.running(ctx, id)
	if err != nil {
		return nil, err
	}
	r, err := vm.OpenFile(ctx, path)
	if err != nil {
		return nil, err
	}
	_ = m.cfg.Store.TouchSandbox(ctx, id, m.now())
	return r, nil
}

// WriteFile writes one guest file.
func (m *Manager) WriteFile(ctx context.Context, id, path string, body io.Reader) error {
	vm, err := m.running(ctx, id)
	if err != nil {
		return err
	}
	if err := vm.WriteFile(ctx, path, body); err != nil {
		return err
	}
	_ = m.cfg.Store.TouchSandbox(ctx, id, m.now())
	return nil
}
