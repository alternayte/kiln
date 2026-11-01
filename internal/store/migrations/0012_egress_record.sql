-- 0012: the egress record, observe mode and the tenant cap on it. A record
-- outlives the sandbox row, because deleting a template removes the rows of its
-- destroyed sandboxes and a caller still reads the record for 7 days. An
-- applied migration is never edited.

ALTER TABLE sandboxes ADD COLUMN egress_observe INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tenants ADD COLUMN no_observe INTEGER NOT NULL DEFAULT 0;

CREATE TABLE egress_logs (
  sandbox_id   TEXT PRIMARY KEY,
  tenant_id    TEXT NOT NULL,
  dropped      INTEGER NOT NULL DEFAULT 0,
  destroyed_at INTEGER
);

CREATE TABLE egress_records (
  sandbox_id TEXT NOT NULL REFERENCES egress_logs(sandbox_id) ON DELETE CASCADE,
  host       TEXT NOT NULL,
  port       INTEGER NOT NULL,
  decision   TEXT NOT NULL,
  count      INTEGER NOT NULL,
  bytes_out  INTEGER NOT NULL,
  bytes_in   INTEGER NOT NULL,
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL,
  PRIMARY KEY (sandbox_id, host, port, decision)
);
