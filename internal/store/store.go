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
	CreateTemplate(ctx context.Context, t Template) error
	SetTemplateState(ctx context.Context, name, state, message string) error
	GetTemplate(ctx context.Context, name string) (Template, error)
	ListTemplates(ctx context.Context) ([]Template, error)
	DeleteTemplate(ctx context.Context, name string) error
	AppendEvent(ctx context.Context, e Event) error
	ListEvents(ctx context.Context, sandboxID string) ([]Event, error)
	Close() error
}
