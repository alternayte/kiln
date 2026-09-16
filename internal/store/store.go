// Package store is the SQLite persistence layer. It is one of the three
// permitted interfaces. SQLite is the only implementation.
package store

import (
	"context"
	"errors"
	"time"
)

// Template build states.
const (
	TemplateBuilding = "building"
	TemplateReady    = "ready"
	TemplateFailed   = "failed"
)

// Sandbox lifecycle values.
const (
	LifecycleEphemeral  = "ephemeral"
	LifecyclePersistent = "persistent"
)

// Sandbox states.
const (
	SandboxCreating  = "creating"
	SandboxRunning   = "running"
	SandboxSleeping  = "sleeping"
	SandboxWaking    = "waking"
	SandboxStopping  = "stopping"
	SandboxDestroyed = "destroyed"
	SandboxFailed    = "failed"
)

// Preview visibility. A public preview needs no credential; a team preview
// needs a viewer session.
const (
	VisibilityPublic = "public"
	VisibilityTeam   = "team"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrConflict is returned when the requested change clashes with the current
// state, such as a second build of the same template.
var ErrConflict = errors.New("store: conflict")

// ErrExhausted is returned when a resource pool has no free slot.
var ErrExhausted = errors.New("store: exhausted")

// Template is one row of the templates table. EgressAllow is never nil.
type Template struct {
	// TenantID owns the template. It names the directory of its files, so
	// two tenants with one template name never share a rootfs.
	TenantID    string
	Name        string
	ImageRef    string
	ImageDigest string
	VCPUs       int
	MemoryMB    int
	DiskMB      int
	EgressAllow []string
	State       string
	Error       string
	CreatedAt   time.Time
}

// Event is one row of the events table.
type Event struct {
	ID        int64
	SandboxID string
	FromState string
	ToState   string
	Reason    string
	At        time.Time
}

// Sandbox is one row of the sandboxes table.
type Sandbox struct {
	ID           string
	TemplateName string
	SnapshotID   string
	Lifecycle    string
	State        string
	TTLSeconds   *int
	IdleSeconds  int
	LastActiveAt time.Time
	TapName      string
	VsockCID     *int
	PID          int
	Metadata     string
	CreatedAt    time.Time
	DestroyedAt  *time.Time
	// SecretBearing marks a sandbox that holds injected secrets in memory.
	SecretBearing bool
}

// Snapshot is one row of the snapshots table. Listed is false for an image a
// fork writes for its own children; those never appear in a listing.
type Snapshot struct {
	ID              string
	TemplateName    string
	ParentID        string
	SizeBytes       int64
	CreatedAt       time.Time
	SecretBearing   bool
	Listed          bool
	OriginSandboxID string
}

// Published is one hostname a sandbox port is served on.
type Published struct {
	SandboxID string
	// TenantID owns the hostname. The ingress reads it to match a viewer
	// session to the preview.
	TenantID   string
	GuestPort  int
	Subdomain  string
	Visibility string
	CreatedAt  time.Time
}

// MinVsockCID is the first guest CID the pool hands out. The host is CID 2.
const MinVsockCID = 3

// MaxVsockCID bounds the pool. One host cannot run a million microVMs, and
// the bound keeps the allocator total.
const MaxVsockCID = 1 << 20

// Store records intent. The host records reality; the reconciler corrects the
// store. SQLite is the only implementation.
type Store interface {
	// CreateTemplate inserts one template row. A duplicate name is an error.
	CreateTemplate(ctx context.Context, t Template) error
	// StartTemplateBuild inserts a template row in the building state. An
	// existing row that is building or ready is ErrConflict; a failed one is
	// replaced.
	StartTemplateBuild(ctx context.Context, t Template) error
	SetTemplateDigest(ctx context.Context, name, digest string) error
	SetTemplateState(ctx context.Context, name, state, message string) error
	GetTemplate(ctx context.Context, name string) (Template, error)
	ListTemplates(ctx context.Context) ([]Template, error)
	// TemplateDependents counts the live sandboxes and the snapshot rows that
	// reference the template. Tables that a later phase adds count as zero.
	TemplateDependents(ctx context.Context, name string) (liveSandboxes, snapshots int, err error)
	DeleteTemplate(ctx context.Context, name string) error
	AppendEvent(ctx context.Context, e Event) error
	ListEvents(ctx context.Context, sandboxID string) ([]Event, error)
	// ListEventsSince returns every event with an id greater than afterID,
	// oldest first. The events endpoint polls it.
	ListEventsSince(ctx context.Context, afterID int64) ([]Event, error)
	// LastEventID returns the newest event id, or zero when there are none.
	LastEventID(ctx context.Context) (int64, error)

	// CreateSandbox inserts one row. A nil VsockCID takes the next free CID
	// from the pool, and the stored row is returned with the value set.
	CreateSandbox(ctx context.Context, s Sandbox) (Sandbox, error)
	GetSandbox(ctx context.Context, id string) (Sandbox, error)
	ListSandboxes(ctx context.Context) ([]Sandbox, error)
	SetSandboxState(ctx context.Context, id, state string) error
	SetSandboxRuntime(ctx context.Context, id, tapName string, vsockCID *int, pid int) error
	TouchSandbox(ctx context.Context, id string, at time.Time) error
	DestroySandbox(ctx context.Context, id string, at time.Time) error

	CreateSnapshot(ctx context.Context, s Snapshot) error
	// GetSnapshot returns a row whether or not it is listed.
	GetSnapshot(ctx context.Context, id string) (Snapshot, error)
	// ListSnapshots returns only the listed rows.
	ListSnapshots(ctx context.Context) ([]Snapshot, error)
	DeleteSnapshot(ctx context.Context, id string) error
	// CountRestoredChildren counts live sandboxes restored from one snapshot.
	CountRestoredChildren(ctx context.Context, snapshotID string) (int, error)

	// PublishSandbox inserts one preview. A subdomain another row already
	// holds is ErrConflict, and the caller draws a new one.
	PublishSandbox(ctx context.Context, p Published) error
	// GetPublished returns one preview by sandbox and port.
	GetPublished(ctx context.Context, sandboxID string, port int) (Published, error)
	// ListPublished returns every preview of one sandbox, oldest first.
	ListPublished(ctx context.Context, sandboxID string) ([]Published, error)
	// PublishedBySubdomain returns the preview one hostname serves.
	PublishedBySubdomain(ctx context.Context, subdomain string) (Published, error)
	// RetirePublished removes one hostname. It 404s from then on and is
	// never handed out again.
	RetirePublished(ctx context.Context, sandboxID string, port int) error
	// DeletePublishedForSandbox removes every hostname of one sandbox.
	DeletePublishedForSandbox(ctx context.Context, sandboxID string) error

	// CreateTenant inserts one tenant with its caps. A duplicate id is
	// ErrConflict.
	CreateTenant(ctx context.Context, t Tenant) error
	GetTenant(ctx context.Context, id string) (Tenant, error)
	ListTenants(ctx context.Context) ([]Tenant, error)
	// SetTenantCaps replaces the caps of one tenant. A zero cap is no limit.
	SetTenantCaps(ctx context.Context, id string, caps Caps) error
	// DeleteTenant removes a tenant that owns no template and no live
	// sandbox. Anything left is ErrConflict.
	DeleteTenant(ctx context.Context, id string) error
	// TenantUsage counts what one tenant holds now.
	TenantUsage(ctx context.Context, id string) (Usage, error)
	Close() error
}

// Tenant owns the rows one caller reaches. A self-hosted install runs the
// DefaultTenant and never creates another.
type Tenant struct {
	ID        string
	Name      string
	Caps      Caps
	CreatedAt time.Time
}

// Caps bound what one tenant holds on the host. A zero value is no limit.
type Caps struct {
	MaxSandboxes     int
	MaxTemplates     int
	MaxSnapshotBytes int64
}

// Usage is what one tenant holds now. The host compares it with Caps before
// it admits a template, a sandbox or a snapshot.
type Usage struct {
	Sandboxes     int
	Templates     int
	SnapshotBytes int64
}

// Viewer is one preview login of a tenant. It lives here because the API
// and the ingress both name it, and neither imports the other.
type Viewer struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}
