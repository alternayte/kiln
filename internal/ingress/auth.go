package ingress

import (
	"context"
	"database/sql"
	"errors"
	"time"

	authall "github.com/alternayte/auth-all"
	"github.com/alternayte/auth-all/apierr"
	"github.com/alternayte/auth-all/ratelimit"
	authsqlite "github.com/alternayte/auth-all/store/sqlite"
)

// NewAuth opens the viewer login on the same SQLite file as the rest of the
// store. The session cookie is scoped to the zone, so one login covers every
// preview. It applies the Auth-All migrations, so a first run creates the
// viewer tables.
func NewAuth(ctx context.Context, database, zone string) (*authall.Auth, *sql.DB, error) {
	db, err := authsqlite.Open(database)
	if err != nil {
		return nil, nil, err
	}
	opts := []authall.Option{
		authall.WithStore(authsqlite.New(db)),
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
	return auth, db, nil
}

// CreateViewer creates one viewer account. It is how the operator makes the
// first account at init; there is no self-signup.
func CreateViewer(ctx context.Context, auth *authall.Auth, address, password string) error {
	_, err := auth.CreateUser(ctx, authall.CreateUserInput{
		Email:         address,
		Password:      password,
		EmailVerified: true,
	})
	return err
}

// ViewerExists reports whether a viewer account with this address exists.
func ViewerExists(ctx context.Context, auth *authall.Auth, address string) (bool, error) {
	_, err := auth.GetUserByEmail(ctx, address)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, apierr.ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}
