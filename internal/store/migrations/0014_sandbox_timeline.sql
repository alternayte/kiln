-- 0014: timeline points. A point row names files under its sandbox directory and
-- ends with its sandbox. timeline_diff_ok says the live VM's dirty pages count from
-- the newest point, so the next point can be a diff. An applied migration is
-- never edited.

ALTER TABLE templates ADD COLUMN timeline INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sandboxes ADD COLUMN timeline_diff_ok INTEGER NOT NULL DEFAULT 0;

CREATE TABLE timeline_points (
  id             TEXT PRIMARY KEY,
  sandbox_id     TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
  tenant_id      TEXT NOT NULL,
  seq            INTEGER NOT NULL,
  label          TEXT NOT NULL,
  kind           TEXT NOT NULL,
  size_bytes     INTEGER NOT NULL,
  merged         INTEGER NOT NULL DEFAULT 0,
  secret_bearing INTEGER NOT NULL,
  egress_allow   TEXT,
  created_at     INTEGER NOT NULL,
  UNIQUE (sandbox_id, seq)
);
CREATE INDEX timeline_points_tenant ON timeline_points(tenant_id);
