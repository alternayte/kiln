package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// Audit records one row per proxied call. It answers who did what, in which
// tenant, and with which credential.
type Audit struct {
	DB *sql.DB
	// Driver is "sqlite" or "postgres". The two speak different DDL and
	// different parameter markers.
	Driver string
}

// Entry is one proxied call.
type Entry struct {
	TenantID string
	UserID   string
	KeyID    string
	Method   string
	Path     string
	Status   int
	Duration time.Duration
	At       time.Time
}

// EnsureSchema creates the table. The gateway owns this store, so it creates
// its own table instead of asking the operator for a migration step.
func (a *Audit) EnsureSchema(ctx context.Context) error {
	if a == nil || a.DB == nil {
		return nil
	}
	id := "INTEGER PRIMARY KEY AUTOINCREMENT"
	if a.Driver == "postgres" {
		id = "BIGSERIAL PRIMARY KEY"
	}
	_, err := a.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS kiln_audit (
		id          `+id+`,
		tenant_id   TEXT NOT NULL,
		user_id     TEXT,
		key_id      TEXT,
		method      TEXT NOT NULL,
		path        TEXT NOT NULL,
		status      INTEGER NOT NULL,
		duration_ms INTEGER NOT NULL,
		at          INTEGER NOT NULL
	)`)
	if err != nil {
		return err
	}
	_, err = a.DB.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS kiln_audit_tenant ON kiln_audit(tenant_id, id)`)
	return err
}

// Record writes one row. A failed write is logged and never fails the call
// the caller already received.
func (a *Audit) Record(ctx context.Context, e Entry) {
	if a == nil || a.DB == nil {
		return
	}
	_, err := a.DB.ExecContext(context.WithoutCancel(ctx), a.rebind(`INSERT INTO kiln_audit (
		tenant_id, user_id, key_id, method, path, status, duration_ms, at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		e.TenantID, nullString(e.UserID), nullString(e.KeyID), e.Method, e.Path,
		e.Status, e.Duration.Milliseconds(), e.At.Unix())
	if err != nil {
		log.Printf("gateway: audit: %v", err)
	}
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// rebind turns the ? markers into the $n markers Postgres expects.
func (a *Audit) rebind(query string) string {
	if a.Driver != "postgres" {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
