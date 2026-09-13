-- 0001: templates and events. The first migration. An applied migration is
-- never edited.

CREATE TABLE templates (
  name           TEXT PRIMARY KEY,
  image_ref      TEXT NOT NULL,
  image_digest   TEXT NOT NULL,
  vcpus          INTEGER NOT NULL,
  memory_mb      INTEGER NOT NULL,
  disk_mb        INTEGER NOT NULL,
  egress_allow   TEXT NOT NULL,
  state          TEXT NOT NULL,
  error          TEXT,
  created_at     INTEGER NOT NULL
);

CREATE TABLE events (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  sandbox_id     TEXT,
  from_state     TEXT,
  to_state       TEXT,
  reason         TEXT,
  at             INTEGER NOT NULL
);
