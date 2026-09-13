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
	"time"

	_ "modernc.org/sqlite"
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
	}
	return nil
}

func (s *sqliteStore) CreateTemplate(ctx context.Context, t Template) error {
	encoded, err := encodeEgress(t.EgressAllow)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO templates (
		name, image_ref, image_digest, vcpus, memory_mb, disk_mb,
		egress_allow, state, error, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Name, t.ImageRef, t.ImageDigest, t.VCPUs, t.MemoryMB, t.DiskMB,
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
	err = tx.QueryRowContext(ctx, `SELECT state FROM templates WHERE name = ?`, t.Name).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO templates (
			name, image_ref, image_digest, vcpus, memory_mb, disk_mb,
			egress_allow, state, error, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.Name, t.ImageRef, t.ImageDigest, t.VCPUs, t.MemoryMB, t.DiskMB,
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
		WHERE name = ?`,
			t.ImageRef, t.ImageDigest, t.VCPUs, t.MemoryMB, t.DiskMB,
			encoded, t.State, t.CreatedAt.Unix(), t.Name,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *sqliteStore) SetTemplateDigest(ctx context.Context, name, digest string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE templates SET image_digest = ? WHERE name = ?`, digest, name)
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
	res, err := s.db.ExecContext(ctx,
		`UPDATE templates SET state = ?, error = ? WHERE name = ?`,
		state, nullString(message), name)
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
	row := s.db.QueryRowContext(ctx, `SELECT
		name, image_ref, image_digest, vcpus, memory_mb, disk_mb,
		egress_allow, state, error, created_at
	FROM templates WHERE name = ?`, name)
	t, err := scanTemplate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Template{}, ErrNotFound
	}
	return t, err
}

func (s *sqliteStore) ListTemplates(ctx context.Context) ([]Template, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		name, image_ref, image_digest, vcpus, memory_mb, disk_mb,
		egress_allow, state, error, created_at
	FROM templates ORDER BY name`)
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

func (s *sqliteStore) DeleteTemplate(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM templates WHERE name = ?`, name)
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

// TemplateDependents counts the rows that keep a template alive. The sandboxes
// and snapshots tables arrive in later phases; a table that does not exist yet
// holds nothing.
func (s *sqliteStore) TemplateDependents(ctx context.Context, name string) (int, int, error) {
	live, err := s.countWhere(ctx, "sandboxes",
		`SELECT count(*) FROM sandboxes WHERE template_name = ? AND destroyed_at IS NULL`, name)
	if err != nil {
		return 0, 0, err
	}
	snapshots, err := s.countWhere(ctx, "snapshots",
		`SELECT count(*) FROM snapshots WHERE template_name = ?`, name)
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
		sandbox_id, from_state, to_state, reason, at
	) VALUES (?, ?, ?, ?, ?)`,
		nullString(e.SandboxID), nullString(e.FromState), nullString(e.ToState), nullString(e.Reason), e.At.Unix())
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
	query += ` ORDER BY id`
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
