package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync"

	authall "github.com/alternayte/auth-all"
	"github.com/alternayte/auth-all/plugins/organizations"
	"github.com/alternayte/auth-all/plugins/organizations/permission"
	authstore "github.com/alternayte/auth-all/store"

	"github.com/alternayte/kiln/internal/apispec"
)

// TenantRoles are the roles a member of a tenant holds. The statements name
// what each apispec operation's Permission asks for.
func TenantRoles() organizations.Option {
	return organizations.Roles(
		organizations.Role("operator", "*"),
		organizations.Role("editor", "sandbox:*", "template:*", "snapshot:*", "organization:read"),
		organizations.Role("reader", "sandbox:read", "template:read", "snapshot:read", "organization:read"),
	)
}

var (
	routesOnce sync.Once
	routes     *http.ServeMux
	routeOps   map[string]apispec.Operation
)

// operationOf returns the API operation one call reaches, matched with the
// patterns the host registers, so the gateway and the host agree on which
// route a path names.
func operationOf(r *http.Request) (apispec.Operation, bool) {
	routesOnce.Do(func() {
		routes = http.NewServeMux()
		routeOps = map[string]apispec.Operation{}
		for _, op := range apispec.Operations() {
			pattern := op.Method + " " + op.Path
			routes.Handle(pattern, http.NotFoundHandler())
			routeOps[pattern] = op
		}
	})
	_, pattern := routes.Handler(r)
	op, ok := routeOps[pattern]
	return op, ok
}

// permits reports whether the caller's role in one tenant holds a statement.
func (s *Server) permits(r *http.Request, tenant, statement string) (bool, error) {
	ctx := r.Context()
	p := authall.PrincipalFrom(ctx)
	if p == nil {
		return false, nil
	}
	if p.APIKey != nil && p.APIKey.Role != "" {
		// A key with its own role carries the intersection of that role and
		// the live role of the member who made it. Can would add that member's
		// whole role back, so the key's statements alone decide. A key with
		// no role acts with the member's role, which Can reads below.
		if p.Membership == nil || p.Membership.Status != authstore.MembershipActive || p.Membership.OrgID != tenant {
			return false, nil
		}
		set, err := permission.NewSet(strings.Fields(p.Membership.Permissions)...)
		if err != nil {
			return false, nil
		}
		return set.Allows(statement), nil
	}
	if active, ok := organizations.From(ctx); ok && active.Organization != nil && active.Organization.ID == tenant {
		return s.Tenants.Can(ctx, statement), nil
	}
	// An access token names a person, so the role comes from the membership
	// of the tenant the call resolved to.
	if p.User == nil {
		return false, nil
	}
	return s.membershipPermits(ctx, p.User.ID, tenant, statement)
}

func (s *Server) membershipPermits(ctx context.Context, userID, tenant, statement string) (bool, error) {
	members, ok := s.Auth.Store().(authstore.MembershipStore)
	if !ok {
		return false, nil
	}
	held, err := members.MembershipsOfUser(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, one := range held {
		if one.OrgID != tenant || one.Status != authstore.MembershipActive {
			continue
		}
		set, _ := s.Tenants.PermissionsOf(one.Role)
		for _, role := range strings.Fields(one.TeamRoles) {
			if team, ok := s.Tenants.PermissionsOf(role); ok {
				set = set.Union(team)
			}
		}
		return set.Allows(statement), nil
	}
	return false, nil
}

// authorize refuses a call the caller's role does not cover. It writes the
// refusal and reports false. An operator route is never forwarded: the
// operator administers tenants through /operator.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, tenant string, op apispec.Operation) bool {
	if op.Operator || op.Permission == "" {
		writeError(w, http.StatusForbidden, "forbidden", "a tenant caller cannot reach "+op.ID)
		return false
	}
	ok, err := s.permits(r, tenant, op.Permission)
	if err != nil {
		writeError(w, http.StatusBadGateway, "internal", "the role lookup failed")
		return false
	}
	if !ok {
		writeError(w, http.StatusForbidden, "forbidden", "the caller's role does not hold "+op.Permission)
		return false
	}
	return true
}
