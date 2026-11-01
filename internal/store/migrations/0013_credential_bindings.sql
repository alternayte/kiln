-- 0013: credential bindings, the tenant CA that terminates TLS on a bound host,
-- and the per-request log of a bound host. A binding row holds a sealed value and
-- ends with its sandbox. The request log lives as long as the egress record. An
-- applied migration is never edited.

ALTER TABLE templates ADD COLUMN trusts_ca INTEGER NOT NULL DEFAULT 0;
ALTER TABLE egress_logs ADD COLUMN requests_dropped INTEGER NOT NULL DEFAULT 0;

CREATE TABLE tenant_cas (
  tenant_id  TEXT PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  cert_pem   TEXT NOT NULL,
  key_sealed TEXT NOT NULL
);

CREATE TABLE credential_bindings (
  sandbox_id   TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  host         TEXT NOT NULL,
  header       TEXT NOT NULL,
  value_sealed TEXT NOT NULL,
  path         TEXT NOT NULL,
  methods      TEXT NOT NULL,
  PRIMARY KEY (sandbox_id, name)
);

CREATE TABLE egress_requests (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  sandbox_id TEXT NOT NULL REFERENCES egress_logs(sandbox_id) ON DELETE CASCADE,
  method     TEXT NOT NULL,
  host       TEXT NOT NULL,
  path       TEXT NOT NULL,
  status     INTEGER NOT NULL,
  binding    TEXT NOT NULL,
  at         INTEGER NOT NULL
);
CREATE INDEX egress_requests_sandbox ON egress_requests(sandbox_id, id);

-- The binding name joins the key of a record, so each key has its own row.
CREATE TABLE egress_records_new (
  sandbox_id TEXT NOT NULL REFERENCES egress_logs(sandbox_id) ON DELETE CASCADE,
  host       TEXT NOT NULL,
  port       INTEGER NOT NULL,
  decision   TEXT NOT NULL,
  binding    TEXT NOT NULL DEFAULT '',
  count      INTEGER NOT NULL,
  bytes_out  INTEGER NOT NULL,
  bytes_in   INTEGER NOT NULL,
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL,
  PRIMARY KEY (sandbox_id, host, port, decision, binding)
);
INSERT INTO egress_records_new (sandbox_id, host, port, decision, count, bytes_out, bytes_in, first_seen, last_seen)
SELECT sandbox_id, host, port, decision, count, bytes_out, bytes_in, first_seen, last_seen FROM egress_records;
DROP TABLE egress_records;
ALTER TABLE egress_records_new RENAME TO egress_records;
