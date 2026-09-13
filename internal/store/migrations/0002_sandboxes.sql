-- 0002: sandboxes. An applied migration is never edited. The snapshots table
-- arrives in P4; this reference is resolved then.

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
