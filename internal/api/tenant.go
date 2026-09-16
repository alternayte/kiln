package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/store"
)

// TenantHeader names the tenant of one call. The gateway sets it after it
// authenticates the caller. The host trusts the name and enforces what that
// tenant reaches.
const TenantHeader = "X-Kiln-Tenant"

// tenant reads the header, checks the tenant exists, and returns a context
// scoped to it. A call with no header is an operator call on the local
// listener, and it reaches every tenant.
func (s *Server) tenant(r *http.Request) (context.Context, error) {
	id := r.Header.Get(TenantHeader)
	if id == "" {
		return r.Context(), nil
	}
	if _, err := s.Store.GetTenant(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: tenant %q does not exist", store.ErrNotFound, id)
		}
		return nil, err
	}
	return store.WithTenant(r.Context(), id), nil
}

// admit refuses a call that would take a tenant over a cap. A zero cap is no
// limit, and a context with no tenant is the operator, who has none.
func (s *Server) admit(ctx context.Context, want resource) error {
	id, ok := store.TenantFrom(ctx)
	if !ok {
		return nil
	}
	tenant, err := s.Store.GetTenant(ctx, id)
	if err != nil {
		return err
	}
	usage, err := s.Store.TenantUsage(ctx, id)
	if err != nil {
		return err
	}
	switch want {
	case wantTemplate:
		if tenant.Caps.MaxTemplates > 0 && usage.Templates >= tenant.Caps.MaxTemplates {
			return fmt.Errorf("%w: the tenant holds %d templates, and its cap is %d",
				sandbox.ErrExhausted, usage.Templates, tenant.Caps.MaxTemplates)
		}
	case wantSandbox:
		if tenant.Caps.MaxSandboxes > 0 && usage.Sandboxes >= tenant.Caps.MaxSandboxes {
			return fmt.Errorf("%w: the tenant runs %d sandboxes, and its cap is %d",
				sandbox.ErrExhausted, usage.Sandboxes, tenant.Caps.MaxSandboxes)
		}
	case wantSnapshot:
		if tenant.Caps.MaxSnapshotBytes > 0 && usage.SnapshotBytes >= tenant.Caps.MaxSnapshotBytes {
			return fmt.Errorf("%w: the tenant holds %d snapshot bytes, and its cap is %d",
				sandbox.ErrExhausted, usage.SnapshotBytes, tenant.Caps.MaxSnapshotBytes)
		}
	}
	return nil
}

// admitCount refuses a fork or a restore that would take a tenant over its
// sandbox cap, because those create more than one sandbox in one call.
func (s *Server) admitCount(ctx context.Context, count int) error {
	id, ok := store.TenantFrom(ctx)
	if !ok {
		return nil
	}
	tenant, err := s.Store.GetTenant(ctx, id)
	if err != nil {
		return err
	}
	if tenant.Caps.MaxSandboxes <= 0 {
		return nil
	}
	usage, err := s.Store.TenantUsage(ctx, id)
	if err != nil {
		return err
	}
	if usage.Sandboxes+count > tenant.Caps.MaxSandboxes {
		return fmt.Errorf("%w: the tenant runs %d sandboxes, asks for %d more, and its cap is %d",
			sandbox.ErrExhausted, usage.Sandboxes, count, tenant.Caps.MaxSandboxes)
	}
	return nil
}

type resource int

const (
	wantTemplate resource = iota
	wantSandbox
	wantSnapshot
)
