package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type sqliteStore struct {
	db *sql.DB
}

// Open opens the SQLite file, sets WAL mode, and applies pending migrations.
func Open(file string) (Store, error) {
	db, err := sql.Open("sqlite", file)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", file, err)
	}
	// One writer, no lock contention inside this process.
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA foreign_keys=ON`,
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: %s: %w", pragma, err)
		}
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &sqliteStore{db: db}, nil
}

// migrate applies each numbered SQL file in order and records it. An applied
// migration is never run twice and never edited.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var applied int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM schema_migrations WHERE version = ?`, name).Scan(&applied); err != nil {
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		if applied > 0 {
			continue
		}
		body, err := fs.ReadFile(migrationsFS, path.Join("migrations", name))
		if err != nil {
			return err
		}
		// A migration that rebuilds a table drops a table other tables
		// reference. The pragma is a no-op inside a transaction, so it goes
		// here, and foreign_key_check below proves the result is sound.
		if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, name, time.Now().Unix()); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		var orphan string
		err = db.QueryRowContext(ctx, `SELECT "table" FROM pragma_foreign_key_check LIMIT 1`).Scan(&orphan)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("store: migration %s: foreign key check: %w", name, err)
		default:
			return fmt.Errorf("store: migration %s: %s holds a row with no parent", name, orphan)
		}
	}
	return nil
}

func (s *sqliteStore) CreateTemplate(ctx context.Context, t Template) error {
	encoded, err := encodeEgress(t.EgressAllow)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO templates (
		tenant_id, name, image_ref, image_digest, vcpus, memory_mb, disk_mb,
		egress_allow, state, error, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tenantOf(ctx), t.Name, t.ImageRef, t.ImageDigest, t.VCPUs, t.MemoryMB, t.DiskMB,
		encoded, t.State, nullString(t.Error), t.CreatedAt.Unix(),
	)
	return err
}

// StartTemplateBuild inserts the building row for a new build. A failed row is
// replaced; a building or ready row is a conflict.
func (s *sqliteStore) StartTemplateBuild(ctx context.Context, t Template) error {
	encoded, err := encodeEgress(t.EgressAllow)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx,
		`SELECT state FROM templates WHERE tenant_id = ? AND name = ?`, tenantOf(ctx), t.Name).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO templates (
			tenant_id, name, image_ref, image_digest, vcpus, memory_mb, disk_mb,
			egress_allow, state, error, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			tenantOf(ctx), t.Name, t.ImageRef, t.ImageDigest, t.VCPUs, t.MemoryMB, t.DiskMB,
			encoded, t.State, nullString(t.Error), t.CreatedAt.Unix(),
		); err != nil {
			return err
		}
	case err != nil:
		return err
	case state == TemplateBuilding || state == TemplateReady:
		return ErrConflict
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE templates SET
			image_ref = ?, image_digest = ?, vcpus = ?, memory_mb = ?, disk_mb = ?,
			egress_allow = ?, state = ?, error = NULL, created_at = ?
		WHERE tenant_id = ? AND name = ?`,
			t.ImageRef, t.ImageDigest, t.VCPUs, t.MemoryMB, t.DiskMB,
			encoded, t.State, t.CreatedAt.Unix(), tenantOf(ctx), t.Name,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *sqliteStore) SetTemplateDigest(ctx context.Context, name, digest string) error {
	clause, args := scope(ctx, "tenant_id")
	res, err := s.db.ExecContext(ctx,
		`UPDATE templates SET image_digest = ? WHERE name = ?`+clause, append([]any{digest, name}, args...)...)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) SetTemplateState(ctx context.Context, name, state, message string) error {
	clause, args := scope(ctx, "tenant_id")
	res, err := s.db.ExecContext(ctx,
		`UPDATE templates SET state = ?, error = ? WHERE name = ?`+clause,
		append([]any{state, nullString(message), name}, args...)...)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) GetTemplate(ctx context.Context, name string) (Template, error) {
	clause, args := scope(ctx, "tenant_id")
	row := s.db.QueryRowContext(ctx, `SELECT
		name, image_ref, image_digest, vcpus, memory_mb, disk_mb,
		egress_allow, state, error, created_at
	FROM templates WHERE name = ?`+clause, append([]any{name}, args...)...)
	t, err := scanTemplate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Template{}, ErrNotFound
	}
	return t, err
}

func (s *sqliteStore) ListTemplates(ctx context.Context) ([]Template, error) {
	clause, args := scope(ctx, "tenant_id")
	rows, err := s.db.QueryContext(ctx, `SELECT
		name, image_ref, image_digest, vcpus, memory_mb, disk_mb,
		egress_allow, state, error, created_at
	FROM templates WHERE 1 = 1`+clause+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTemplate removes a template. The destroyed sandbox rows that name it
// go in the same transaction, because the foreign key blocks the delete
// otherwise. Events are kept.
func (s *sqliteStore) DeleteTemplate(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Published rows reference the sandbox rows, so they go first. Every
	// hostname leaves a tombstone, because a retired hostname is never reused.
	clause, args := scope(ctx, "tenant_id")
	rows, err := tx.QueryContext(ctx, `SELECT subdomain FROM published WHERE sandbox_id IN (
		SELECT id FROM sandboxes WHERE template_name = ? AND destroyed_at IS NOT NULL`+clause+`
	)`, append([]any{name}, args...)...)
	if err != nil {
		return err
	}
	var subdomains []string
	for rows.Next() {
		var subdomain string
		if err := rows.Scan(&subdomain); err != nil {
			rows.Close()
			return err
		}
		subdomains = append(subdomains, subdomain)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if err := retireHostnames(ctx, tx, tenantOf(ctx), subdomains...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM published WHERE sandbox_id IN (
		SELECT id FROM sandboxes WHERE template_name = ? AND destroyed_at IS NOT NULL`+clause+`
	)`, append([]any{name}, args...)...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sandboxes WHERE template_name = ? AND destroyed_at IS NOT NULL`+clause,
		append([]any{name}, args...)...); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM templates WHERE name = ?`+clause, append([]any{name}, args...)...)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// TemplateDependents counts the rows that keep a template alive. The sandboxes
// and snapshots tables arrive in later phases; a table that does not exist yet
// holds nothing.
func (s *sqliteStore) TemplateDependents(ctx context.Context, name string) (int, int, error) {
	clause, args := scope(ctx, "tenant_id")
	live, err := s.countWhere(ctx, "sandboxes",
		`SELECT count(*) FROM sandboxes WHERE template_name = ? AND destroyed_at IS NULL`+clause,
		append([]any{name}, args...)...)
	if err != nil {
		return 0, 0, err
	}
	snapshots, err := s.countWhere(ctx, "snapshots",
		`SELECT count(*) FROM snapshots WHERE template_name = ?`+clause, append([]any{name}, args...)...)
	if err != nil {
		return 0, 0, err
	}
	return live, snapshots, nil
}

func (s *sqliteStore) countWhere(ctx context.Context, table, query string, args ...any) (int, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// encodeEgress stores the allowlist as a JSON array. It is never null.
func encodeEgress(egress []string) (string, error) {
	if egress == nil {
		egress = []string{}
	}
	b, err := json.Marshal(egress)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *sqliteStore) AppendEvent(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO events (
		tenant_id, sandbox_id, from_state, to_state, reason, at
	) VALUES (?, ?, ?, ?, ?, ?)`,
		tenantOf(ctx), nullString(e.SandboxID), nullString(e.FromState), nullString(e.ToState), nullString(e.Reason), e.At.Unix())
	return err
}

func (s *sqliteStore) ListEvents(ctx context.Context, sandboxID string) ([]Event, error) {
	query := `SELECT id, sandbox_id, from_state, to_state, reason, at FROM events`
	var args []any
	if sandboxID == "" {
		query += ` WHERE sandbox_id IS NULL`
	} else {
		query += ` WHERE sandbox_id = ?`
		args = append(args, sandboxID)
	}
	clause, tenantArgs := scope(ctx, "tenant_id")
	query += clause + ` ORDER BY id`
	args = append(args, tenantArgs...)
	return s.queryEvents(ctx, query, args...)
}

// ListEventsSince returns every event after one id, oldest first.
func (s *sqliteStore) ListEventsSince(ctx context.Context, afterID int64) ([]Event, error) {
	clause, args := scope(ctx, "tenant_id")
	return s.queryEvents(ctx,
		`SELECT id, sandbox_id, from_state, to_state, reason, at FROM events WHERE id > ?`+clause+` ORDER BY id`,
		append([]any{afterID}, args...)...)
}

// LastEventID returns the newest event id, or zero when the table is empty.
func (s *sqliteStore) LastEventID(ctx context.Context) (int64, error) {
	var id int64
	clause, args := scope(ctx, "tenant_id")
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(max(id), 0) FROM events WHERE 1 = 1`+clause, args...).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *sqliteStore) queryEvents(ctx context.Context, query string, args ...any) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var (
			e     Event
			sid   sql.NullString
			from  sql.NullString
			to    sql.NullString
			why   sql.NullString
			atSec int64
		)
		if err := rows.Scan(&e.ID, &sid, &from, &to, &why, &atSec); err != nil {
			return nil, err
		}
		e.SandboxID, e.FromState, e.ToState, e.Reason = sid.String, from.String, to.String, why.String
		e.At = time.Unix(atSec, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *sqliteStore) Close() error { return s.db.Close() }

type scanner interface {
	Scan(dest ...any) error
}

func scanTemplate(row scanner) (Template, error) {
	var (
		t       Template
		egress  string
		message sql.NullString
		created int64
	)
	if err := row.Scan(
		&t.Name, &t.ImageRef, &t.ImageDigest, &t.VCPUs, &t.MemoryMB, &t.DiskMB,
		&egress, &t.State, &message, &created,
	); err != nil {
		return Template{}, err
	}
	if err := json.Unmarshal([]byte(egress), &t.EgressAllow); err != nil {
		return Template{}, fmt.Errorf("store: template %s: egress_allow: %w", t.Name, err)
	}
	if t.EgressAllow == nil {
		t.EgressAllow = []string{}
	}
	t.Error = message.String
	t.CreatedAt = time.Unix(created, 0).UTC()
	return t, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullInt stores a zero as NULL.
func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// nullIntPtr stores a nil pointer as NULL.
func nullIntPtr(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// CreateSandbox inserts one row and allocates its vsock CID from the pool in
// the same transaction, so two creates can never take the same CID. A caller
// that sets VsockCID keeps it.
func (s *sqliteStore) CreateSandbox(ctx context.Context, sb Sandbox) (Sandbox, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Sandbox{}, err
	}
	defer tx.Rollback()
	cid := sb.VsockCID
	if cid == nil {
		used := map[int]bool{}
		rows, err := tx.QueryContext(ctx,
			`SELECT vsock_cid FROM sandboxes WHERE destroyed_at IS NULL AND vsock_cid IS NOT NULL`)
		if err != nil {
			return Sandbox{}, err
		}
		for rows.Next() {
			var n int
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return Sandbox{}, err
			}
			used[n] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return Sandbox{}, err
		}
		rows.Close()
		next, err := allocateCID(used, MaxVsockCID)
		if err != nil {
			return Sandbox{}, err
		}
		cid = &next
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sandboxes (
		id, tenant_id, template_name, snapshot_id, lifecycle, state, ttl_seconds, idle_seconds,
		last_active_at, tap_name, vsock_cid, pid, metadata, created_at, secret_bearing
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sb.ID, tenantOf(ctx), sb.TemplateName, nullString(sb.SnapshotID), sb.Lifecycle, sb.State,
		nullIntPtr(sb.TTLSeconds), sb.IdleSeconds, sb.LastActiveAt.Unix(),
		nullString(sb.TapName), nullIntPtr(cid), nullInt(sb.PID), sb.Metadata, sb.CreatedAt.Unix(),
		boolInt(sb.SecretBearing),
	); err != nil {
		return Sandbox{}, err
	}
	if err := tx.Commit(); err != nil {
		return Sandbox{}, err
	}
	sb.VsockCID = cid
	return sb, nil
}

// allocateCID returns the lowest free CID in the pool. Exhaustion is an
// error, never a repeated CID.
func allocateCID(used map[int]bool, max int) (int, error) {
	for cid := MinVsockCID; cid <= max; cid++ {
		if !used[cid] {
			return cid, nil
		}
	}
	return 0, ErrExhausted
}

func (s *sqliteStore) GetSandbox(ctx context.Context, id string) (Sandbox, error) {
	clause, args := scope(ctx, "tenant_id")
	row := s.db.QueryRowContext(ctx, `SELECT
		id, template_name, snapshot_id, lifecycle, state, ttl_seconds, idle_seconds,
		last_active_at, tap_name, vsock_cid, pid, metadata, created_at, destroyed_at, secret_bearing
	FROM sandboxes WHERE id = ?`+clause, append([]any{id}, args...)...)
	sb, err := scanSandbox(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	return sb, err
}

func (s *sqliteStore) ListSandboxes(ctx context.Context) ([]Sandbox, error) {
	clause, args := scope(ctx, "tenant_id")
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, template_name, snapshot_id, lifecycle, state, ttl_seconds, idle_seconds,
		last_active_at, tap_name, vsock_cid, pid, metadata, created_at, destroyed_at, secret_bearing
	FROM sandboxes WHERE 1 = 1`+clause+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

func (s *sqliteStore) SetSandboxState(ctx context.Context, id, state string) error {
	clause, args := scope(ctx, "tenant_id")
	res, err := s.db.ExecContext(ctx,
		`UPDATE sandboxes SET state = ? WHERE id = ?`+clause, append([]any{state, id}, args...)...)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) SetSandboxRuntime(ctx context.Context, id, tapName string, vsockCID *int, pid int) error {
	clause, args := scope(ctx, "tenant_id")
	res, err := s.db.ExecContext(ctx,
		`UPDATE sandboxes SET tap_name = ?, vsock_cid = ?, pid = ? WHERE id = ?`+clause,
		append([]any{nullString(tapName), nullIntPtr(vsockCID), nullInt(pid), id}, args...)...)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) TouchSandbox(ctx context.Context, id string, at time.Time) error {
	clause, args := scope(ctx, "tenant_id")
	_, err := s.db.ExecContext(ctx,
		`UPDATE sandboxes SET last_active_at = ? WHERE id = ?`+clause, append([]any{at.Unix(), id}, args...)...)
	return err
}

func (s *sqliteStore) DestroySandbox(ctx context.Context, id string, at time.Time) error {
	clause, args := scope(ctx, "tenant_id")
	res, err := s.db.ExecContext(ctx, `UPDATE sandboxes SET
		state = ?, destroyed_at = COALESCE(destroyed_at, ?), pid = NULL
	WHERE id = ?`+clause, append([]any{SandboxDestroyed, at.Unix(), id}, args...)...)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

type sandboxScanner interface {
	Scan(dest ...any) error
}

func scanSandbox(row sandboxScanner) (Sandbox, error) {
	var (
		sb        Sandbox
		snapshot  sql.NullString
		ttl       sql.NullInt64
		active    int64
		tap       sql.NullString
		cid       sql.NullInt64
		pid       sql.NullInt64
		created   int64
		destroyed sql.NullInt64
		secret    int
	)
	if err := row.Scan(
		&sb.ID, &sb.TemplateName, &snapshot, &sb.Lifecycle, &sb.State, &ttl, &sb.IdleSeconds,
		&active, &tap, &cid, &pid, &sb.Metadata, &created, &destroyed, &secret,
	); err != nil {
		return Sandbox{}, err
	}
	sb.SnapshotID = snapshot.String
	sb.LastActiveAt = time.Unix(active, 0).UTC()
	if ttl.Valid {
		v := int(ttl.Int64)
		sb.TTLSeconds = &v
	}
	sb.TapName = tap.String
	if cid.Valid {
		v := int(cid.Int64)
		sb.VsockCID = &v
	}
	sb.PID = int(pid.Int64)
	sb.CreatedAt = time.Unix(created, 0).UTC()
	if destroyed.Valid {
		t := time.Unix(destroyed.Int64, 0).UTC()
		sb.DestroyedAt = &t
	}
	sb.SecretBearing = secret != 0
	return sb, nil
}

// boolInt stores a bool as the 0 or 1 SQLite uses.
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *sqliteStore) CreateSnapshot(ctx context.Context, sn Snapshot) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO snapshots (
		id, tenant_id, template_name, parent_id, size_bytes, created_at,
		secret_bearing, listed, origin_sandbox_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sn.ID, tenantOf(ctx), sn.TemplateName, nullString(sn.ParentID), sn.SizeBytes, sn.CreatedAt.Unix(),
		boolInt(sn.SecretBearing), boolInt(sn.Listed), nullString(sn.OriginSandboxID),
	)
	return err
}

func (s *sqliteStore) GetSnapshot(ctx context.Context, id string) (Snapshot, error) {
	clause, args := scope(ctx, "tenant_id")
	row := s.db.QueryRowContext(ctx, `SELECT
		id, template_name, parent_id, size_bytes, created_at,
		secret_bearing, listed, origin_sandbox_id
	FROM snapshots WHERE id = ?`+clause, append([]any{id}, args...)...)
	sn, err := scanSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	return sn, err
}

func (s *sqliteStore) ListSnapshots(ctx context.Context) ([]Snapshot, error) {
	clause, args := scope(ctx, "tenant_id")
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, template_name, parent_id, size_bytes, created_at,
		secret_bearing, listed, origin_sandbox_id
	FROM snapshots WHERE listed = 1`+clause+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		sn, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// DeleteSnapshot removes one image row. A sandbox row that was restored from
// it keeps its history, so the reference is cleared in the same transaction
// the foreign key needs.
func (s *sqliteStore) DeleteSnapshot(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	clause, args := scope(ctx, "tenant_id")
	if _, err := tx.ExecContext(ctx,
		`UPDATE sandboxes SET snapshot_id = NULL WHERE snapshot_id = ?`+clause,
		append([]any{id}, args...)...); err != nil {
		return err
	}
	// parent_id is lineage only: a child snapshot holds its own files, so it
	// outlives its parent.
	if _, err := tx.ExecContext(ctx,
		`UPDATE snapshots SET parent_id = NULL WHERE parent_id = ?`+clause,
		append([]any{id}, args...)...); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ?`+clause, append([]any{id}, args...)...)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *sqliteStore) CountRestoredChildren(ctx context.Context, snapshotID string) (int, error) {
	clause, args := scope(ctx, "tenant_id")
	return s.countWhere(ctx, "sandboxes",
		`SELECT count(*) FROM sandboxes WHERE snapshot_id = ? AND destroyed_at IS NULL`+clause,
		append([]any{snapshotID}, args...)...)
}

func (s *sqliteStore) PublishSandbox(ctx context.Context, p Published) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var retired int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM retired_hostnames WHERE subdomain = ?`, p.Subdomain).Scan(&retired); err != nil {
		return err
	}
	if retired > 0 {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO published (
		sandbox_id, tenant_id, guest_port, subdomain, visibility, created_at
	) VALUES (?, ?, ?, ?, ?, ?)`,
		p.SandboxID, tenantOf(ctx), p.GuestPort, p.Subdomain, p.Visibility, p.CreatedAt.Unix()); err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	}
	return tx.Commit()
}

func (s *sqliteStore) GetPublished(ctx context.Context, sandboxID string, port int) (Published, error) {
	clause, args := scope(ctx, "tenant_id")
	row := s.db.QueryRowContext(ctx, `SELECT
		sandbox_id, tenant_id, guest_port, subdomain, visibility, created_at
	FROM published WHERE sandbox_id = ? AND guest_port = ?`+clause, append([]any{sandboxID, port}, args...)...)
	p, err := scanPublished(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Published{}, ErrNotFound
	}
	return p, err
}

func (s *sqliteStore) ListPublished(ctx context.Context, sandboxID string) ([]Published, error) {
	clause, args := scope(ctx, "tenant_id")
	rows, err := s.db.QueryContext(ctx, `SELECT
		sandbox_id, tenant_id, guest_port, subdomain, visibility, created_at
	FROM published WHERE sandbox_id = ?`+clause+` ORDER BY created_at, guest_port`,
		append([]any{sandboxID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Published
	for rows.Next() {
		p, err := scanPublished(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *sqliteStore) PublishedBySubdomain(ctx context.Context, subdomain string) (Published, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		sandbox_id, tenant_id, guest_port, subdomain, visibility, created_at
	FROM published WHERE subdomain = ?`, subdomain)
	p, err := scanPublished(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Published{}, ErrNotFound
	}
	return p, err
}

func (s *sqliteStore) RetirePublished(ctx context.Context, sandboxID string, port int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	clause, args := scope(ctx, "tenant_id")
	var subdomain string
	err = tx.QueryRowContext(ctx,
		`SELECT subdomain FROM published WHERE sandbox_id = ? AND guest_port = ?`+clause,
		append([]any{sandboxID, port}, args...)...).Scan(&subdomain)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := retireHostnames(ctx, tx, tenantOf(ctx), subdomain); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM published WHERE sandbox_id = ? AND guest_port = ?`+clause,
		append([]any{sandboxID, port}, args...)...); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *sqliteStore) DeletePublishedForSandbox(ctx context.Context, sandboxID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	clause, args := scope(ctx, "tenant_id")
	rows, err := tx.QueryContext(ctx,
		`SELECT subdomain FROM published WHERE sandbox_id = ?`+clause, append([]any{sandboxID}, args...)...)
	if err != nil {
		return err
	}
	var subdomains []string
	for rows.Next() {
		var subdomain string
		if err := rows.Scan(&subdomain); err != nil {
			rows.Close()
			return err
		}
		subdomains = append(subdomains, subdomain)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, subdomain := range subdomains {
		if err := retireHostnames(ctx, tx, tenantOf(ctx), subdomain); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM published WHERE sandbox_id = ?`+clause, append([]any{sandboxID}, args...)...); err != nil {
		return err
	}
	return tx.Commit()
}

// retireHostnames writes the tombstones that make each hostname unreusable.
// Retiring an already retired hostname is not an error.
func retireHostnames(ctx context.Context, tx *sql.Tx, tenant string, subdomains ...string) error {
	for _, subdomain := range subdomains {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO retired_hostnames (subdomain, tenant_id, retired_at) VALUES (?, ?, ?)
			 ON CONFLICT(subdomain) DO NOTHING`, subdomain, tenant, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

// isUniqueViolation reports whether a modernc.org/sqlite error is a unique or
// primary key conflict.
func isUniqueViolation(err error) bool {
	var e *sqlite.Error
	if errors.As(err, &e) {
		// 2067 is SQLITE_CONSTRAINT_UNIQUE and 1555 is SQLITE_CONSTRAINT_PRIMARYKEY.
		if e.Code() == 2067 || e.Code() == 1555 {
			return true
		}
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func scanPublished(row scanner) (Published, error) {
	var (
		p       Published
		created int64
	)
	if err := row.Scan(&p.SandboxID, &p.TenantID, &p.GuestPort, &p.Subdomain, &p.Visibility, &created); err != nil {
		return Published{}, err
	}
	p.CreatedAt = time.Unix(created, 0).UTC()
	return p, nil
}

func scanSnapshot(row sandboxScanner) (Snapshot, error) {
	var (
		sn      Snapshot
		parent  sql.NullString
		created int64
		secret  int
		listed  int
		origin  sql.NullString
	)
	if err := row.Scan(
		&sn.ID, &sn.TemplateName, &parent, &sn.SizeBytes, &created,
		&secret, &listed, &origin,
	); err != nil {
		return Snapshot{}, err
	}
	sn.ParentID = parent.String
	sn.CreatedAt = time.Unix(created, 0).UTC()
	sn.SecretBearing = secret != 0
	sn.Listed = listed != 0
	sn.OriginSandboxID = origin.String
	return sn, nil
}

func (s *sqliteStore) CreateTenant(ctx context.Context, t Tenant) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tenants (
		id, name, max_sandboxes, max_templates, max_snapshot_bytes, created_at
	) VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Caps.MaxSandboxes, t.Caps.MaxTemplates, t.Caps.MaxSnapshotBytes, t.CreatedAt.Unix())
	if err != nil && isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (s *sqliteStore) GetTenant(ctx context.Context, id string) (Tenant, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, name, max_sandboxes, max_templates, max_snapshot_bytes, created_at
	FROM tenants WHERE id = ?`, id)
	t, err := scanTenant(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	return t, err
}

func (s *sqliteStore) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, name, max_sandboxes, max_templates, max_snapshot_bytes, created_at
	FROM tenants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *sqliteStore) SetTenantCaps(ctx context.Context, id string, caps Caps) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tenants SET
		max_sandboxes = ?, max_templates = ?, max_snapshot_bytes = ?
	WHERE id = ?`, caps.MaxSandboxes, caps.MaxTemplates, caps.MaxSnapshotBytes, id)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteTenant refuses while the tenant still owns anything, because the rows
// name host files that only a destroy removes.
func (s *sqliteStore) DeleteTenant(ctx context.Context, id string) error {
	usage, err := s.TenantUsage(ctx, id)
	if err != nil {
		return err
	}
	if usage.Sandboxes > 0 || usage.Templates > 0 {
		return ErrConflict
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM tenants WHERE id = ?`, id)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) TenantUsage(ctx context.Context, id string) (Usage, error) {
	var u Usage
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sandboxes WHERE tenant_id = ? AND destroyed_at IS NULL`, id).Scan(&u.Sandboxes); err != nil {
		return Usage{}, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM templates WHERE tenant_id = ?`, id).Scan(&u.Templates); err != nil {
		return Usage{}, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(sum(size_bytes), 0) FROM snapshots WHERE tenant_id = ?`, id).Scan(&u.SnapshotBytes); err != nil {
		return Usage{}, err
	}
	return u, nil
}

func scanTenant(row scanner) (Tenant, error) {
	var (
		t       Tenant
		created int64
	)
	if err := row.Scan(&t.ID, &t.Name, &t.Caps.MaxSandboxes, &t.Caps.MaxTemplates,
		&t.Caps.MaxSnapshotBytes, &created); err != nil {
		return Tenant{}, err
	}
	t.CreatedAt = time.Unix(created, 0).UTC()
	return t, nil
}
