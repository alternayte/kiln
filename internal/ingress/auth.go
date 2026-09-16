package ingress

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/alternayte/kiln/internal/store"

	authall "github.com/alternayte/auth-all"
	"github.com/alternayte/auth-all/apierr"
	"github.com/alternayte/auth-all/plugins/admin"
	"github.com/alternayte/auth-all/plugins/organizations"
	"github.com/alternayte/auth-all/plugins/roles"
	"github.com/alternayte/auth-all/ratelimit"
	authstore "github.com/alternayte/auth-all/store"
	authsqlite "github.com/alternayte/auth-all/store/sqlite"
)

// ViewerRole is the only role a viewer holds. A viewer opens the team
// previews of one tenant and reaches nothing else.
const ViewerRole = "viewer"

// viewerAdminRole exists so the admin plugin registers. No viewer holds it,
// and no route grants it.
const viewerAdminRole = "viewer-admin"

// Viewers is the viewer store of this host. A viewer belongs to one tenant,
// and the ingress refuses a session of another tenant. The tenant id of the
// host store is the slug here, because the plugin makes its own ids.
type Viewers struct {
	Auth     *authall.Auth
	accounts *admin.Plugin
	tenants  *organizations.Plugin
	store    authstore.OrganizationStore
}

// NewAuth opens the viewer login on the same SQLite file as the rest of the
// store. The session cookie is scoped to the zone, so one login covers every
// preview. It applies the Auth-All migrations, so a first run creates the
// viewer tables.
func NewAuth(ctx context.Context, database, zone string) (*Viewers, *sql.DB, error) {
	db, err := authsqlite.Open(database)
	if err != nil {
		return nil, nil, err
	}
	// One tenant row here holds the viewers of one tenant. The ids match the
	// tenant ids of the host store, so the ingress compares them directly.
	tenants := organizations.New(
		organizations.Roles(organizations.Role(ViewerRole, "preview:read")),
		organizations.DefaultRole(ViewerRole),
		organizations.OwnerRole(ViewerRole),
	)
	// The admin plugin disables a viewer the tenant revokes. Auth-All has no
	// public delete, and the last-owner guard refuses a membership removal
	// here, because every viewer owns its own tenant row.
	// The viewer store has no administrator who signs in; the plugin is here
	// only so the host can disable a revoked viewer. The role exists in the
	// hierarchy and nobody holds it.
	accounts := admin.New(admin.AdminRole(viewerAdminRole))
	opts := []authall.Option{
		authall.WithStore(authsqlite.New(db)),
		// The admin plugin needs the roles plugin registered before it.
		authall.WithPlugins(roles.New(roles.Hierarchy(ViewerRole, viewerAdminRole)), tenants, accounts),
		authall.WithBasePath(AuthPrefix),
		authall.WithEmailPassword(),
		// No email flow exists in v1, so an address is proven by the operator
		// who creates the account.
		authall.WithRateLimiter(ratelimit.NewMemory(20, time.Minute)),
	}
	if zone != "" {
		opts = append(opts,
			authall.WithBaseURL("https://"+zone),
			authall.WithCookie(authall.CookieOptions{Domain: "." + zone}),
		)
	}
	auth, err := authall.New(opts...)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	if _, err := auth.Migrate(ctx); err != nil {
		db.Close()
		return nil, nil, err
	}
	orgStore, ok := auth.Store().(authstore.OrganizationStore)
	if !ok {
		db.Close()
		return nil, nil, errors.New("ingress: the viewer store holds no tenant")
	}
	viewers := &Viewers{Auth: auth, accounts: accounts, tenants: tenants, store: orgStore}
	if err := viewers.adoptOldViewers(ctx); err != nil {
		db.Close()
		return nil, nil, err
	}
	return viewers, db, nil
}

// Create makes one viewer of one tenant. The first viewer of a tenant
// creates the tenant row here, and every later one joins it.
func (v *Viewers) Create(ctx context.Context, tenant, address, password string) error {
	user, err := v.Auth.CreateUser(ctx, authall.CreateUserInput{
		Email:         address,
		Password:      password,
		EmailVerified: true,
	})
	if err != nil {
		return err
	}
	return v.join(ctx, tenant, user)
}

// row returns the tenant row of this viewer store.
func (v *Viewers) row(ctx context.Context, tenant string) (*authstore.Organization, error) {
	return v.store.OrganizationBySlug(ctx, slugOf(tenant))
}

// Belongs reports whether one viewer belongs to one tenant. The ingress asks
// before it forwards a team preview.
func (v *Viewers) Belongs(ctx context.Context, tenant, userID string) bool {
	if v == nil || v.tenants == nil {
		return false
	}
	row, err := v.row(ctx, tenant)
	if err != nil {
		return false
	}
	_, _, _, err = v.tenants.KeyCredential(ctx, row.ID, userID, "")
	return err == nil
}

// List returns the viewers of one tenant.
func (v *Viewers) List(ctx context.Context, tenant string) ([]store.Viewer, error) {
	row, err := v.row(ctx, tenant)
	if err != nil {
		return nil, err
	}
	page, err := v.tenants.ListMembers(ctx, row.ID, authstore.MemberFilter{})
	if err != nil {
		return nil, err
	}
	out := make([]store.Viewer, 0, len(page.Members))
	for _, member := range page.Members {
		user, err := v.Auth.GetUser(ctx, member.UserID)
		if err != nil {
			continue
		}
		out = append(out, store.Viewer{ID: user.ID, Email: user.Email, CreatedAt: user.CreatedAt})
	}
	return out, nil
}

// Delete removes one viewer of one tenant. A viewer belongs to one tenant,
// so the login goes with the membership.
func (v *Viewers) Delete(ctx context.Context, tenant, userID string) error {
	row, err := v.row(ctx, tenant)
	if err != nil {
		return err
	}
	// The tenant is checked first, or one tenant would delete another's
	// viewer by guessing an id.
	if !v.Belongs(ctx, tenant, userID) {
		return apierr.ErrNotFound
	}
	_ = row
	if _, err := v.accounts.Disable(ctx, userID); err != nil {
		return err
	}
	// A disabled viewer keeps an open session until it is revoked.
	_, err = v.Auth.RevokeUserSessions(ctx, userID)
	return err
}

// slugOf turns a tenant id into the slug the plugin accepts.
func slugOf(tenant string) string {
	out := make([]rune, 0, len(tenant))
	for _, r := range tenant {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+32)
		default:
			out = append(out, '-')
		}
	}
	if len(out) == 0 || out[0] == '-' {
		out = append([]rune{'t'}, out...)
	}
	if len(out) > 63 {
		out = out[:63]
	}
	return string(out)
}

// CreateViewer creates one viewer of the default tenant. It is how the
// operator makes the first one at init; there is no self-signup.
func CreateViewer(ctx context.Context, viewers *Viewers, address, password string) error {
	return viewers.Create(ctx, store.DefaultTenant, address, password)
}

// ViewerExists reports whether this address already holds a viewer.
func ViewerExists(ctx context.Context, viewers *Viewers, address string) (bool, error) {
	return viewers.Exists(ctx, address)
}

// Exists reports whether an address already holds a viewer.
func (v *Viewers) Exists(ctx context.Context, address string) (bool, error) {
	_, err := v.Auth.GetUserByEmail(ctx, address)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, apierr.ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

// adoptOldViewers gives every viewer from before tenants a place in the
// default tenant. Without it a host that upgrades refuses the viewer the
// operator already created.
func (v *Viewers) adoptOldViewers(ctx context.Context) error {
	lister, ok := v.Auth.Store().(authstore.UserAdminStore)
	if !ok {
		return nil
	}
	cursor := ""
	for {
		users, next, err := lister.ListUsers(ctx, authstore.UserListFilter{Limit: 100, Cursor: cursor})
		if err != nil {
			return err
		}
		members, ok := v.Auth.Store().(authstore.MembershipStore)
		if !ok {
			return nil
		}
		for i := range users {
			held, err := members.MembershipsOfUser(ctx, users[i].ID)
			if err != nil {
				return err
			}
			if len(held) > 0 {
				continue
			}
			if err := v.join(ctx, store.DefaultTenant, &users[i]); err != nil {
				return err
			}
		}
		if next == "" || len(users) == 0 {
			return nil
		}
		cursor = next
	}
}

// join puts one viewer in one tenant, and creates the tenant row when this
// viewer is its first. A later viewer is written straight to the store: the
// plugin's SetRole speaks for an actor who already holds rights here, and a
// new viewer holds none.
func (v *Viewers) join(ctx context.Context, tenant string, user *authstore.User) error {
	row, err := v.row(ctx, tenant)
	if err != nil {
		_, err = v.tenants.Create(ctx, user, organizations.CreateInput{
			Name: tenant, Slug: slugOf(tenant), OwnerID: user.ID,
		})
		return err
	}
	members, ok := v.Auth.Store().(authstore.MembershipStore)
	if !ok {
		return errors.New("ingress: the viewer store holds no membership")
	}
	if _, err := members.MembershipOf(ctx, row.ID, user.ID); err == nil {
		return nil
	}
	return members.CreateMembership(ctx, &authstore.Membership{
		ID:       newMembershipID(),
		OrgID:    row.ID,
		UserID:   user.ID,
		Role:     ViewerRole,
		Status:   authstore.MembershipActive,
		JoinedAt: time.Now().UTC(),
	})
}

// newMembershipID returns the identifier one membership row takes.
func newMembershipID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// A host with no entropy cannot serve previews either.
		panic("ingress: " + err.Error())
	}
	return hex.EncodeToString(raw[:])
}
