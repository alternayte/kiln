// Command kiln-gateway is the public control API of Kiln. It authenticates a
// caller, names the tenant, and proxies the call to a Kiln host over mTLS.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	authall "github.com/alternayte/auth-all"
	"github.com/alternayte/auth-all/plugins/apikeys"
	"github.com/alternayte/auth-all/plugins/oauthprovider"
	"github.com/alternayte/auth-all/plugins/organizations"
	"github.com/alternayte/auth-all/plugins/roles"
	"github.com/alternayte/auth-all/ratelimit"
	"github.com/alternayte/auth-all/ratelimit/storelimit"

	"github.com/alternayte/kiln/internal/gateway"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "kiln-gateway: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		return migrate()
	}
	addr := env("KILN_GATEWAY_ADDR", ":8080")
	baseURL := env("KILN_GATEWAY_URL", "http://localhost"+addr)

	auth, tenants, keys, provider, db, err := newAuth(baseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx0 := context.Background()

	// The store is this gateway's own, and the migration is idempotent, so a
	// start applies what is pending. A deployment then needs no second step,
	// and an image with no shell needs no command run inside it.
	if env("KILN_SKIP_MIGRATE", "") == "" {
		applied, err := auth.Migrate(ctx0)
		if err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		if len(applied) > 0 {
			fmt.Printf("kiln-gateway: applied %d statements\n", len(applied))
		}
	}

	host, err := gateway.NewHost(hostCredentials())
	if err != nil {
		return err
	}
	audit := &gateway.Audit{DB: db.DB, Driver: db.Driver}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := audit.EnsureSchema(ctx); err != nil {
		return err
	}

	server := &gateway.Server{
		Auth:         auth,
		Tenants:      tenants,
		Keys:         keys,
		Host:         host,
		Audit:        audit,
		AuthPrefix:   authPrefix + "/",
		OAuth:        provider,
		Issuer:       strings.TrimSuffix(baseURL, "/") + authPrefix,
		BaseURL:      baseURL,
		OperatorRole: env("KILN_OPERATOR_ROLE", "operator"),
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// The authorization server keeps spent codes and expired tokens until
	// something removes them.
	go func() {
		tick := time.NewTicker(time.Hour)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := server.Cleanup(ctx); err != nil {
					fmt.Fprintf(os.Stderr, "kiln-gateway: cleanup: %v\n", err)
				}
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	fmt.Printf("kiln-gateway: listening on %s for host %s\n", addr, host.URL)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// migrate creates the auth-all tables. It runs once before the first start.
func migrate() error {
	auth, _, _, _, db, err := newAuth(env("KILN_GATEWAY_URL", "http://localhost:8080"))
	if err != nil {
		return err
	}
	defer db.Close()
	applied, err := auth.Migrate(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("kiln-gateway: applied %d statements\n", len(applied))
	return nil
}

// newAuth builds the auth stack this gateway runs on. One auth-all tenant row
// is one tenant, and a key of that row is a machine credential for it.
// authPrefix is where the login and OAuth routes live.
const authPrefix = "/auth"

func newAuth(baseURL string) (*authall.Auth, *organizations.Plugin, *apikeys.Plugin, *oauthprovider.Plugin, *sqlDB, error) {
	db, err := openStore()
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	tenants := organizations.New(
		organizations.Roles(
			organizations.Role("operator", "*"),
			organizations.Role("editor", "sandbox:*", "template:*", "snapshot:*", "organization:read"),
			organizations.Role("viewer", "sandbox:read", "template:read", "snapshot:read", "organization:read"),
		),
		organizations.DefaultRole("editor"),
		organizations.OwnerRole("operator"),
	)
	keys := apikeys.New(apikeys.Prefix("kiln_"), apikeys.Organizations(tenants))
	// The authorization server an MCP client talks to. A client registers
	// itself, so an agent needs no console and no static credential.
	kek, err := oauthKey()
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, nil, err
	}
	provider := oauthprovider.New(
		oauthprovider.KeyEncryptionKey(kek),
		oauthprovider.LoginPath(env("KILN_LOGIN_PATH", authPrefix+"/sign-in")),
		oauthprovider.ConsentPath(env("KILN_CONSENT_PATH", authPrefix+"/consent")),
		oauthprovider.AllowDynamicRegistration(),
		oauthprovider.Scopes("openid", "email", "sandbox"),
		// The identifier is the issuer, because auth-all accepts its own
		// token only when the audience names the issuer.
		oauthprovider.Resources(oauthprovider.Resource{
			Identifier:     strings.TrimSuffix(baseURL, "/") + authPrefix,
			Scopes:         []string{"openid", "email", "sandbox"},
			AccessTokenTTL: 30 * time.Minute,
		}),
	)
	// The counters live in the gateway store, so every gateway shares one
	// count and a restart forgets nothing.
	limiter, err := storelimit.New(db.authStore(), ratelimit.DefaultSignInRules())
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, nil, err
	}
	auth, err := authall.New(
		authall.WithStore(db.authStore()),
		// The metadata names these paths, so the mount point and the base
		// path have to agree or a client follows a dead endpoint.
		authall.WithBasePath(authPrefix),
		authall.WithRateLimiter(limiter),
		authall.WithBaseURL(baseURL),
		authall.WithEmailPassword(),
		authall.WithPlugins(
			roles.New(roles.Hierarchy("viewer", "editor", "operator")),
			tenants,
			keys,
			provider,
		),
	)
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, nil, err
	}
	return auth, tenants, keys, provider, db, nil
}

// oauthKey reads the 32 bytes that wrap every signing key at rest. A gateway
// with no key refuses to start, because a new key on every restart would
// invalidate every token it issued.
func oauthKey() ([]byte, error) {
	value := strings.TrimSpace(os.Getenv("KILN_OAUTH_KEY"))
	if value == "" {
		return nil, errors.New("KILN_OAUTH_KEY is required: 32 bytes, base64, from your credential store")
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("KILN_OAUTH_KEY: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("KILN_OAUTH_KEY holds %d bytes, want 32", len(raw))
	}
	return raw, nil
}

func hostCredentials() gateway.HostCredentials {
	return gateway.HostCredentials{
		URL:        os.Getenv("KILN_HOST_URL"),
		Token:      os.Getenv("KILN_HOST_TOKEN"),
		CACert:     pem("KILN_CA_CERT"),
		ClientCert: pem("KILN_CLIENT_CERT"),
		ClientKey:  pem("KILN_CLIENT_KEY"),
	}
}

// pem reads one PEM value from the environment, or from the file the
// <NAME>_FILE variable names. A credential store holds either form, and some
// of them keep one line per variable, so base64 and \n escapes both work.
func pem(name string) []byte {
	if path := os.Getenv(name + "_FILE"); path != "" {
		body, err := os.ReadFile(path)
		if err == nil {
			return body
		}
	}
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil
	}
	if !strings.Contains(value, "-----BEGIN") {
		if raw, err := base64.StdEncoding.DecodeString(value); err == nil {
			return raw
		}
	}
	return []byte(strings.ReplaceAll(value, `\n`, "\n"))
}

func env(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
