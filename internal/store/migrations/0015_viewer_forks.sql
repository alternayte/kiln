-- 0015: a published origin that gives each viewer login its own fork. The origin
-- row holds the one snapshot every fork restores from, and each fork row names
-- the current fork of one viewer. An applied migration is never edited.

CREATE TABLE viewer_origins (
  sandbox_id  TEXT PRIMARY KEY REFERENCES sandboxes(id) ON DELETE CASCADE,
  snapshot_id TEXT NOT NULL,
  viewer_cap  INTEGER NOT NULL,
  bindings    TEXT NOT NULL,
  created_at  TEXT NOT NULL
);
CREATE INDEX viewer_origins_snapshot ON viewer_origins(snapshot_id);

CREATE TABLE viewer_forks (
  origin_id TEXT NOT NULL REFERENCES viewer_origins(sandbox_id) ON DELETE CASCADE,
  viewer_id TEXT NOT NULL,
  fork_id   TEXT NOT NULL,
  PRIMARY KEY (origin_id, viewer_id)
);
