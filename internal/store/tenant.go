package store

import "context"

// DefaultTenant is the tenant of a self-hosted install. Migration 0006 creates
// it and gives every existing row to it.
const DefaultTenant = "default"

type tenantKey struct{}

// WithTenant scopes every store call made with the returned context to one
// tenant. The API sets it from the gateway's header. A context without a
// tenant reaches every row, which is what the operator token and the
// reconciler need.
func WithTenant(ctx context.Context, tenant string) context.Context {
	if tenant == "" {
		return ctx
	}
	return context.WithValue(ctx, tenantKey{}, tenant)
}

// TenantFrom returns the tenant of a context, and whether it has one.
func TenantFrom(ctx context.Context) (string, bool) {
	tenant, ok := ctx.Value(tenantKey{}).(string)
	return tenant, ok && tenant != ""
}

// scope returns the SQL predicate and argument that limit a statement to the
// context's tenant. Without a tenant it returns an empty clause, so one query
// serves both the scoped caller and the operator.
func scope(ctx context.Context, column string) (string, []any) {
	tenant, ok := TenantFrom(ctx)
	if !ok {
		return "", nil
	}
	return " AND " + column + " = ?", []any{tenant}
}

// tenantOf returns the tenant to write on a new row. A context without a
// tenant writes the default tenant, so a local operator call still names one.
func tenantOf(ctx context.Context) string {
	if tenant, ok := TenantFrom(ctx); ok {
		return tenant
	}
	return DefaultTenant
}
