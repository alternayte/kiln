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

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrConflict is returned when the requested change clashes with the current
// state, such as a second build of the same template.
var ErrConflict = errors.New("store: conflict")

// Template is one row of the templates table. EgressAllow is never nil.
type Template struct {
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
	Close() error
}
