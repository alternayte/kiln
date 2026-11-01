package template

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alternayte/kiln/internal/guestproto"
	"github.com/alternayte/kiln/internal/hostca"
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
	// TTLSeconds ends the template with no client involved. Nil is no
	// deadline.
	TTLSeconds *int
	// Start runs the application, and StartPort is the port it listens on.
	Start     []string
	StartPort int
	// User is who everything a caller asks for runs as. Setup stays root,
	// because the image and its setup need it.
	User string
	// Services run beside the application, and Seed runs once they are ready.
	Services []ServiceRequest
	Seed     []string
	// Credential pulls the images of this build. It lives in memory for the
	// build and reaches no row, no manifest, no event and no log.
	Credential *Credential
}

// ServiceRequest is one image that runs beside the application.
type ServiceRequest struct {
	Name  string
	Image string
	Env   map[string]string
	Port  int
	Start []string
	Ready []string
}

// Credential is the registry login of one build. The registry is the host of
// the image reference.
type Credential struct {
	Username string
	Token    string
}

// userPattern is a user name or a numeric id, as an image's passwd file holds
// it.
var servicePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// maxServices bounds one template. Each service is an image pull and a process
// in one microVM.
const maxServices = 16

var userPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)

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
	// A start with no port has no readiness. A port with no start runs the
	// command the image names.
	if len(r.Start) > 0 && r.StartPort == 0 {
		return invalidf("start needs a port")
	}
	if r.StartPort < 0 || r.StartPort > 65535 {
		return invalidf("port must be between 1 and 65535")
	}
	if r.User != "" && !userPattern.MatchString(r.User) {
		return invalidf("user %q must match %s", r.User, userPattern)
	}
	if err := r.validateServices(); err != nil {
		return err
	}
	for i, cmd := range r.Seed {
		if strings.TrimSpace(cmd) == "" {
			return invalidf("seed[%d] is empty", i)
		}
	}
	if r.Credential != nil {
		if r.Credential.Username == "" || r.Credential.Token == "" {
			return invalidf("credential needs a username and a token")
		}
		if _, err := withBuildCredential(nil, r.Image, r.Credential); err != nil {
			return &InvalidError{Message: err.Error()}
		}
	}
	return nil
}

// validateServices checks the services. They share one network, so one port
// has one owner.
func (r BuildRequest) validateServices() error {
	if len(r.Services) > maxServices {
		return invalidf("a template holds at most %d services", maxServices)
	}
	names := map[string]bool{}
	ports := map[int]string{}
	if r.StartPort != 0 {
		ports[r.StartPort] = "the application"
	}
	for i, service := range r.Services {
		if !servicePattern.MatchString(service.Name) {
			return invalidf("services[%d].name %q must match %s", i, service.Name, servicePattern)
		}
		if names[service.Name] {
			return invalidf("service %s appears twice", service.Name)
		}
		names[service.Name] = true
		if strings.TrimSpace(service.Image) == "" {
			return invalidf("service %s needs an image", service.Name)
		}
		if service.Port < 0 || service.Port > 65535 {
			return invalidf("service %s: port must be between 1 and 65535", service.Name)
		}
		if holder, taken := ports[service.Port]; taken && service.Port != 0 {
			return invalidf("service %s and %s both use port %d", service.Name, holder, service.Port)
		}
		if service.Port != 0 {
			ports[service.Port] = "service " + service.Name
		}
		for key := range service.Env {
			if !envKeyPattern.MatchString(key) {
				return invalidf("service %s: env key %q must match %s", service.Name, key, envKeyPattern)
			}
		}
	}
	return nil
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// stateful says whether the snapshot boot runs anything. Such a template keeps
// the drive of that boot, because its memory has that drive mounted.
func (r BuildRequest) stateful() bool { return len(r.Services) > 0 || len(r.Seed) > 0 }

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
	// SealKey opens the registry tokens this tenant stored.
	SealKey []byte

	// KernelPath is the pinned vmlinux the build VM boots.
	KernelPath string
	// Now returns the current time. Tests set it.
	Now func() time.Time

	nextCID atomic.Uint32

	// buildMu guards building, the names this process is building now. The
	// reconciler must not sweep a live build VM or fail its template.
	buildMu  sync.Mutex
	building map[string]bool
}

// TemplateDir is the on-disk directory of one template. The tenant names the
// parent, so two tenants with one template name never share a rootfs or a
// memory image.
func (m *Manager) TemplateDir(tenant, name string) string {
	if tenant == "" {
		tenant = store.DefaultTenant
	}
	return filepath.Join(m.Root, "templates", tenant, name)
}

// dirOf is the directory of the template this context reaches.
func (m *Manager) dirOf(ctx context.Context, name string) string {
	tenant, _ := store.TenantFrom(ctx)
	return m.TemplateDir(tenant, name)
}

// layerCacheDir is the directory of the layers one tenant has pulled.
func (m *Manager) layerCacheDir(ctx context.Context) string {
	tenant, ok := store.TenantFrom(ctx)
	if !ok {
		tenant = store.DefaultTenant
	}
	return filepath.Join(m.Root, "layers", tenant)
}

// RemoveLayerCache deletes the layers of one tenant. They are its data, so they
// go with it.
func (m *Manager) RemoveLayerCache(tenant string) error {
	return os.RemoveAll(filepath.Join(m.Root, "layers", tenant))
}

// buildKey names one build of one tenant, because two tenants may build one
// template name at the same time.
func buildKey(ctx context.Context, name string) string {
	tenant, ok := store.TenantFrom(ctx)
	if !ok {
		tenant = store.DefaultTenant
	}
	return tenant + "/" + name
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
		TTLSeconds:  req.TTLSeconds,
		Start:       req.Start,
		StartPort:   req.StartPort,
		User:        req.User,
	}
	for _, service := range req.Services {
		row.Services = append(row.Services, store.TemplateService{Name: service.Name, Image: service.Image, Port: service.Port})
	}
	if m.IsBuilding(buildKey(ctx, req.Name)) {
		return store.ErrConflict
	}
	// Protect the build before the row lands, so a sweep that runs in between
	// never reads a building row as stale.
	m.setBuilding(buildKey(ctx, req.Name), true)
	if err := m.Store.StartTemplateBuild(ctx, row); err != nil {
		m.setBuilding(buildKey(ctx, req.Name), false)
		return err
	}
	if err := m.Store.AppendEvent(ctx, store.Event{
		FromState: "",
		ToState:   store.TemplateBuilding,
		Reason:    "build started",
		At:        now,
		Template:  req.Name,
	}); err != nil {
		// The build itself matters more than its first event.
		log.Printf("template: %s: start event: %v", req.Name, err)
	}
	go m.build(ctx, req)
	return nil
}

// IsBuilding reports whether this process is building one template now.
// The key is the tenant and the name, from buildKey.
func (m *Manager) IsBuilding(key string) bool {
	m.buildMu.Lock()
	defer m.buildMu.Unlock()
	return m.building[key]
}

// Protected returns the internal ids of the build VMs this process runs. The
// reconciler keeps their host resources.
func (m *Manager) Protected() map[string]bool {
	m.buildMu.Lock()
	defer m.buildMu.Unlock()
	out := make(map[string]bool, 2*len(m.building))
	for key := range m.building {
		out[m.buildID(key, "s")] = true
		out[m.buildID(key, "n")] = true
	}
	return out
}

func (m *Manager) setBuilding(key string, on bool) {
	m.buildMu.Lock()
	defer m.buildMu.Unlock()
	if m.building == nil {
		m.building = map[string]bool{}
	}
	if on {
		m.building[key] = true
		return
	}
	delete(m.building, key)
}

// TemplateInfo returns the row with its snapshot size and child count.
func (m *Manager) TemplateInfo(ctx context.Context, name string) (Info, error) {
	row, err := m.Store.GetTemplate(ctx, name)
	if err != nil {
		return Info{}, err
	}
	info := Info{Template: row}
	for _, file := range []string{"mem", "state", runtime.OverlayFile} {
		if fi, err := os.Stat(filepath.Join(m.dirOf(ctx, name), file)); err == nil {
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
	if err := os.RemoveAll(m.dirOf(ctx, name)); err != nil {
		return err
	}
	return m.Store.DeleteTemplate(ctx, name)
}

// build runs one build and records its terminal state.
func (m *Manager) build(ctx context.Context, req BuildRequest) {
	defer m.setBuilding(buildKey(ctx, req.Name), false)
	err := m.runBuild(ctx, req)
	now := m.now()
	message := ""
	state := store.TemplateReady
	reason := "template ready"
	if err != nil {
		state = store.TemplateFailed
		message = err.Error()
		reason = "build failed"
		if rerr := os.RemoveAll(m.dirOf(ctx, req.Name)); rerr != nil {
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
		Template:  req.Name,
	}); eerr != nil {
		log.Printf("template: %s: event: %v", req.Name, eerr)
	}
}

func (m *Manager) runBuild(ctx context.Context, req BuildRequest) (err error) {
	dir := m.dirOf(ctx, req.Name)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")
	// The builder is shared, and credentials are not: each build takes a copy
	// carrying only the credentials of the tenant it belongs to.
	builder := m.Builder
	creds, err := m.credentialsFor(ctx)
	if err != nil {
		return err
	}
	if builder.Credentials, err = withBuildCredential(creds, req.Image, req.Credential); err != nil {
		return err
	}
	phase := func(name string, began time.Time, err error) { m.phaseEvent(ctx, req.Name, name, began, err, "") }
	builder.LayerCacheDir = m.layerCacheDir(ctx)
	imported, err := builder.Import(ctx, ImportSpec{
		Ref: req.Image, RootfsPath: rootfs, SizeMB: req.DiskMB, Services: req.Services,
		Phase: func(name string, began time.Time, err error, note string) {
			m.phaseEvent(ctx, req.Name, name, began, err, note)
		},
	})
	if err != nil {
		return err
	}
	if err := m.Store.SetTemplateDigest(ctx, req.Name, imported.Digest); err != nil {
		return err
	}
	// A port with no start runs the image's command, so an image with none
	// cannot serve anything.
	if req.StartPort != 0 && len(req.Start) == 0 && len(imported.Config.Command()) == 0 {
		return fmt.Errorf("port %d has no start, and image %s names no command", req.StartPort, req.Image)
	}

	began := time.Now()
	att, err := m.Network.Attach(ctx, m.buildVMID(ctx, req.Name, "s"), network.Policy{Allow: req.EgressAllow})
	if err != nil {
		phase("setup_boot", began, err)
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

	// The setup boot writes the rootfs.
	setupVM, err := m.Runtime.Start(ctx, runtime.Spec{
		ID:             m.buildVMID(ctx, req.Name, "s"),
		KernelPath:     m.KernelPath,
		RootfsPath:     rootfs,
		RootfsReadOnly: false,
		VCPUs:          req.VCPUs,
		MemoryMiB:      req.MemoryMB,
		VsockCID:       m.allocCID(),
		VsockPort:      guestproto.Port,
		TAPName:        att.TAPName,
	})
	phase("setup_boot", began, err)
	if err != nil {
		return err
	}
	began = time.Now()
	if err := m.runSetup(ctx, setupVM, req); err != nil {
		phase("setup", began, err)
		return err
	}
	phase("setup", began, nil)

	// The snapshot boot attaches the finished rootfs read-only and a writable
	// slot for a sandbox overlay. The snapshot then carries both drives, and
	// the rootfs keeps its read-only feature for every restore.
	began = time.Now()
	overlay := filepath.Join(dir, "overlay-slot.ext4")
	snapVM, err := m.bootForSnapshot(ctx, req, rootfs, overlay, att.TAPName)
	phase("snapshot_boot", began, err)
	if err != nil {
		return err
	}
	if req.stateful() {
		if err := m.runStack(ctx, snapVM, req, imported.Config); err != nil {
			_ = m.stopVM(ctx, snapVM)
			return err
		}
	}
	began = time.Now()
	takeSnapshot := m.Runtime.Snapshot
	if req.stateful() {
		takeSnapshot = m.Runtime.SnapshotWithDisk
	}
	if _, err := takeSnapshot(ctx, snapVM, dir); err != nil {
		phase("snapshot", began, err)
		_ = m.stopVM(ctx, snapVM)
		return err
	}
	if err := m.stopVM(ctx, snapVM); err != nil {
		phase("snapshot", began, err)
		return err
	}
	phase("snapshot", began, nil)
	_ = os.Remove(overlay)
	return writeManifest(dir, Manifest{
		Name:        req.Name,
		ImageRef:    req.Image,
		ImageDigest: imported.Digest,
		VCPUs:       req.VCPUs,
		MemoryMB:    req.MemoryMB,
		DiskMB:      req.DiskMB,
		EgressAllow: req.EgressAllow,
		Setup:       req.Setup,
		User:        req.User,
		Services:    servicesOf(req),
		Seed:        req.Seed,
		CreatedAt:   m.now(),
	})
}

func servicesOf(req BuildRequest) []store.TemplateService {
	out := make([]store.TemplateService, 0, len(req.Services))
	for _, service := range req.Services {
		out = append(out, store.TemplateService{Name: service.Name, Image: service.Image, Port: service.Port})
	}
	return out
}

// runStack starts the services in order and runs the seed commands, in the
// boot the template snapshot comes from. The resume hook pivots the guest onto
// the writable drive first, so everything they write lands there.
func (m *Manager) runStack(ctx context.Context, vm *runtime.VM, req BuildRequest, app guestproto.ImageConfig) error {
	entropy := make([]byte, 64)
	if _, err := rand.Read(entropy); err != nil {
		return err
	}
	if err := vm.ResumeHooks(ctx, entropy, time.Now().UnixNano(), "template", nil, ""); err != nil {
		return fmt.Errorf("the writable drive: %w", err)
	}
	began := time.Now()
	for _, service := range req.Services {
		if err := vm.StartService(ctx, runtime.ServiceRequest{
			Name: service.Name, Cmd: service.Start, Env: service.Env, Port: service.Port, Ready: service.Ready,
		}, guestproto.StartDeadlineSeconds); err != nil {
			err = fmt.Errorf("service %s: %w", service.Name, err)
			m.phaseEvent(ctx, req.Name, "services", began, err, "")
			return err
		}
	}
	m.phaseEvent(ctx, req.Name, "services", began, nil, "")

	began = time.Now()
	env := map[string]string{}
	for _, pair := range app.Env {
		if key, value, ok := strings.Cut(pair, "="); ok {
			env[key] = value
		}
	}
	for i, cmd := range req.Seed {
		res, err := vm.Exec(ctx, runtime.ExecRequest{
			Cmd: []string{"sh", "-c", cmd}, Cwd: app.WorkingDir, Env: env, TimeoutSeconds: setupTimeoutSeconds,
		})
		if err == nil && (res.TimedOut || res.ExitCode != 0) {
			err = fmt.Errorf("exit %d timed_out=%v: %s", res.ExitCode, res.TimedOut,
				strings.TrimSpace(strings.Join([]string{res.Stderr, res.Stdout}, "\n")))
		}
		if err != nil {
			err = fmt.Errorf("seed[%d] %q: %w", i, cmd, err)
			m.phaseEvent(ctx, req.Name, "seed", began, err, "")
			return err
		}
	}
	m.phaseEvent(ctx, req.Name, "seed", began, nil, "")
	return nil
}

// runSetup runs the setup commands as root, checks that the template user
// exists once they have run, and stops the VM.
func (m *Manager) runSetup(ctx context.Context, setupVM *runtime.VM, req BuildRequest) error {
	for i, cmd := range req.Setup {
		res, execErr := setupVM.Exec(ctx, runtime.ExecRequest{
			Cmd:            []string{"sh", "-c", cmd},
			TimeoutSeconds: setupTimeoutSeconds,
		})
		if execErr != nil {
			_ = m.stopVM(ctx, setupVM)
			return fmt.Errorf("setup[%d] %q: %w", i, cmd, execErr)
		}
		if res.TimedOut || res.ExitCode != 0 {
			output := strings.TrimSpace(strings.Join([]string{res.Stderr, res.Stdout}, "\n"))
			err := fmt.Errorf("setup[%d] %q: exit %d timed_out=%v: %s", i, cmd, res.ExitCode, res.TimedOut, output)
			if tail := setupVM.ConsoleTail(); tail != "" {
				err = fmt.Errorf("%w; console tail: %s", err, tail)
			}
			_ = m.stopVM(ctx, setupVM)
			return err
		}
	}
	if err := m.trustTenantCA(ctx, setupVM, req); err != nil {
		_ = m.stopVM(ctx, setupVM)
		return err
	}
	// A sandbox from a template with no such user cannot run anything.
	if req.User != "" {
		if err := setupVM.CheckUser(ctx, req.User); err != nil {
			_ = m.stopVM(ctx, setupVM)
			return fmt.Errorf("user %q: %w", req.User, err)
		}
	}
	return m.stopVM(ctx, setupVM)
}

// bootForSnapshot makes the empty overlay slot and boots the finished rootfs.
func (m *Manager) bootForSnapshot(ctx context.Context, req BuildRequest, rootfs, overlay, tap string) (*runtime.VM, error) {
	overlayFile, err := os.OpenFile(overlay, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	if err := overlayFile.Truncate(int64(req.DiskMB) << 20); err != nil {
		overlayFile.Close()
		return nil, err
	}
	if err := overlayFile.Close(); err != nil {
		return nil, err
	}
	if req.stateful() {
		if out, err := exec.CommandContext(ctx, "mke2fs", "-q", "-F", "-t", "ext4", "-L", "kiln-overlay", overlay).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("template: mke2fs: %w: %s", err, out)
		}
	}
	return m.Runtime.Start(ctx, runtime.Spec{
		ID:             m.buildVMID(ctx, req.Name, "n"),
		KernelPath:     m.KernelPath,
		RootfsPath:     rootfs,
		RootfsReadOnly: true,
		OverlayPath:    overlay,
		VCPUs:          req.VCPUs,
		MemoryMiB:      req.MemoryMB,
		VsockCID:       m.allocCID(),
		VsockPort:      guestproto.Port,
		TAPName:        tap,
	})
}

// phaseEvent records how long one step of a build took. A failed step carries
// its error, so a failed build shows where it stopped.
func (m *Manager) phaseEvent(ctx context.Context, name, phase string, began time.Time, failure error, note string) {
	event := store.Event{
		Template:   name,
		Phase:      phase,
		DurationMS: time.Since(began).Milliseconds(),
		At:         m.now(),
		Reason:     note,
	}
	if failure != nil {
		event.Reason = failure.Error()
	}
	if err := m.Store.AppendEvent(context.WithoutCancel(ctx), event); err != nil {
		log.Printf("template: %s: phase event: %v", name, err)
	}
}

// stopVM stops one build VM and reports its error.
func (m *Manager) stopVM(ctx context.Context, vm *runtime.VM) error {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	return m.Runtime.Stop(stopCtx, vm)
}

// buildID is the internal id of one build VM. It has no sandbox row, but its
// TAP, chains and directory carry the id so a sweep can find them. The id is
// short: the jailer puts it twice in the API socket path, and a unix socket
// path is capped at 108 bytes.
// credentialsFor returns the registry credentials of the tenant this build
// belongs to, with their tokens opened. A tenant with none pulls anonymously.
func (m *Manager) credentialsFor(ctx context.Context) ([]RegistryCredential, error) {
	rows, err := m.Store.ListRegistries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RegistryCredential, 0, len(rows))
	for _, row := range rows {
		token, err := store.Unseal(m.SealKey, row.Token)
		if err != nil {
			return nil, fmt.Errorf("template: registry %s: %w", row.Host, err)
		}
		out = append(out, RegistryCredential{Host: row.Host, Username: row.Username, Token: token})
	}
	return out, nil
}

// buildVMID is the id one build gives its VM. The reconciler protects the
// same id while the build runs.
func (m *Manager) buildVMID(ctx context.Context, name, phase string) string {
	// The key names the tenant as well as the template, so two tenants
	// building one name run two VMs, and so the id matches what Protected
	// derives from the same key.
	return m.buildID(buildKey(ctx, name), phase)
}

func (m *Manager) buildID(name, phase string) string {
	sum := sha256.Sum256([]byte(name))
	return "b" + hex.EncodeToString(sum[:5]) + phase
}

// allocCID hands out distinct guest CIDs for concurrent builds.
func (m *Manager) allocCID() uint32 { return m.nextCID.Add(1) + 2 }

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now().UTC()
}

// trustTenantCA adds the tenant CA to the application root, after every setup
// command, so a command that installs or rebuilds the trust store keeps it. A
// host with no sealing key builds a template that takes no credential binding.
func (m *Manager) trustTenantCA(ctx context.Context, vm *runtime.VM, req BuildRequest) error {
	if len(m.SealKey) == 0 {
		return nil
	}
	tenant, ok := store.TenantFrom(ctx)
	if !ok {
		tenant = store.DefaultTenant
	}
	ca, err := hostca.TenantCA(ctx, m.Store, m.SealKey, tenant)
	if err != nil {
		return fmt.Errorf("tenant CA: %w", err)
	}
	if err := vm.TrustCA(ctx, ca.CertPEM); err != nil {
		return fmt.Errorf("trust the tenant CA: %w", err)
	}
	return m.Store.SetTemplateTrustsCA(ctx, req.Name)
}
