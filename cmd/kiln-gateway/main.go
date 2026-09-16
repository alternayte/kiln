// Command kiln-gateway is the public control API of Kiln. It authenticates a
// caller, names the tenant, and proxies the call to a Kiln host over mTLS.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	authall "github.com/alternayte/auth-all"
	"github.com/alternayte/auth-all/plugins/apikeys"
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

	auth, tenants, keys, db, err := newAuth(baseURL)
	if err != nil {
		return err
	}
	defer db.Close()

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

	srv := &http.Server{
		Addr: addr,
		Handler: (&gateway.Server{
			Auth:         auth,
			Tenants:      tenants,
			Keys:         keys,
			Host:         host,
			Audit:        audit,
			AuthPrefix:   "/auth/",
			BaseURL:      baseURL,
			OperatorRole: env("KILN_OPERATOR_ROLE", "operator"),
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
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
	auth, _, _, db, err := newAuth(env("KILN_GATEWAY_URL", "http://localhost:8080"))
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
func newAuth(baseURL string) (*authall.Auth, *organizations.Plugin, *apikeys.Plugin, *sqlDB, error) {
	db, err := openStore()
	if err != nil {
		return nil, nil, nil, nil, err
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
	// The counters live in the gateway store, so every gateway shares one
	// count and a restart forgets nothing.
	limiter, err := storelimit.New(db.authStore(), ratelimit.DefaultSignInRules())
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, err
	}
	auth, err := authall.New(
		authall.WithStore(db.authStore()),
		authall.WithRateLimiter(limiter),
		authall.WithBaseURL(baseURL),
		authall.WithEmailPassword(),
		authall.WithPlugins(
			roles.New(roles.Hierarchy("viewer", "editor", "operator")),
			tenants,
			keys,
		),
	)
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, err
	}
	return auth, tenants, keys, db, nil
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
// <NAME>_FILE variable names. A credential store holds either form.
func pem(name string) []byte {
	if path := os.Getenv(name + "_FILE"); path != "" {
		body, err := os.ReadFile(path)
		if err == nil {
			return body
		}
	}
	value := os.Getenv(name)
	if value == "" {
		return nil
	}
	// A one-line environment value carries the newlines as an escape.
	return []byte(strings.ReplaceAll(value, `\n`, "\n"))
}

func env(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
