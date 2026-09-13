-- 0002: sandboxes. The snapshots table holds no rows before P4, but the
-- sandboxes foreign key needs its parent table to exist. An applied migration
-- is never edited.

CREATE TABLE snapshots (
  id             TEXT PRIMARY KEY,
  template_name  TEXT NOT NULL REFERENCES templates(name),
  parent_id      TEXT REFERENCES snapshots(id),
  size_bytes     INTEGER NOT NULL,
  created_at     INTEGER NOT NULL
);

CREATE TABLE sandboxes (
  id             TEXT PRIMARY KEY,
  template_name  TEXT NOT NULL REFERENCES templates(name),
  snapshot_id    TEXT REFERENCES snapshots(id),
  lifecycle      TEXT NOT NULL,
  state          TEXT NOT NULL,
  ttl_seconds    INTEGER,
  idle_seconds   INTEGER,
  last_active_at INTEGER NOT NULL,
  tap_name       TEXT,
  vsock_cid      INTEGER,
  pid            INTEGER,
  metadata       TEXT NOT NULL DEFAULT '{}',
  created_at     INTEGER NOT NULL,
  destroyed_at   INTEGER
);
