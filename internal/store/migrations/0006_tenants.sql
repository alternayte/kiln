-- 0006: tenants. Every row a caller reaches carries tenant_id, so one query
-- can never return another tenant's rows. A template name is unique inside a
-- tenant, not across the host, so the primary key of templates changes and
-- the tables that reference it are rebuilt with a composite foreign key. A
-- subdomain stays unique across the host, because a hostname is public and is
-- never handed out twice. An applied migration is never edited.

CREATE TABLE tenants (
  id                 TEXT PRIMARY KEY,
  name               TEXT NOT NULL,
  max_sandboxes      INTEGER NOT NULL,
  max_templates      INTEGER NOT NULL,
  max_snapshot_bytes INTEGER NOT NULL,
  created_at         INTEGER NOT NULL
);

-- The install that runs one tenant keeps every row it already has.
INSERT INTO tenants (id, name, max_sandboxes, max_templates, max_snapshot_bytes, created_at)
VALUES ('default', 'default', 0, 0, 0, unixepoch());

CREATE TABLE templates_new (
  tenant_id      TEXT NOT NULL REFERENCES tenants(id),
  name           TEXT NOT NULL,
  image_ref      TEXT NOT NULL,
  image_digest   TEXT NOT NULL,
  vcpus          INTEGER NOT NULL,
  memory_mb      INTEGER NOT NULL,
  disk_mb        INTEGER NOT NULL,
  egress_allow   TEXT NOT NULL,
  state          TEXT NOT NULL,
  error          TEXT,
  created_at     INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, name)
);
INSERT INTO templates_new
SELECT 'default', name, image_ref, image_digest, vcpus, memory_mb, disk_mb,
       egress_allow, state, error, created_at
FROM templates;

CREATE TABLE snapshots_new (
  id                TEXT PRIMARY KEY,
  tenant_id         TEXT NOT NULL REFERENCES tenants(id),
  template_name     TEXT NOT NULL,
  parent_id         TEXT REFERENCES snapshots_new(id),
  size_bytes        INTEGER NOT NULL,
  created_at        INTEGER NOT NULL,
  secret_bearing    INTEGER NOT NULL DEFAULT 0,
  listed            INTEGER NOT NULL DEFAULT 1,
  origin_sandbox_id TEXT,
  FOREIGN KEY (tenant_id, template_name) REFERENCES templates_new(tenant_id, name)
);
INSERT INTO snapshots_new
SELECT id, 'default', template_name, parent_id, size_bytes, created_at,
       secret_bearing, listed, origin_sandbox_id
FROM snapshots;

CREATE TABLE sandboxes_new (
  id             TEXT PRIMARY KEY,
  tenant_id      TEXT NOT NULL REFERENCES tenants(id),
  template_name  TEXT NOT NULL,
  snapshot_id    TEXT REFERENCES snapshots_new(id),
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
  destroyed_at   INTEGER,
  secret_bearing INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY (tenant_id, template_name) REFERENCES templates_new(tenant_id, name)
);
INSERT INTO sandboxes_new
SELECT id, 'default', template_name, snapshot_id, lifecycle, state, ttl_seconds,
       idle_seconds, last_active_at, tap_name, vsock_cid, pid, metadata,
       created_at, destroyed_at, secret_bearing
FROM sandboxes;

CREATE TABLE published_new (
  sandbox_id     TEXT NOT NULL REFERENCES sandboxes_new(id),
  tenant_id      TEXT NOT NULL REFERENCES tenants(id),
  guest_port     INTEGER NOT NULL,
  subdomain      TEXT NOT NULL UNIQUE,
  visibility     TEXT NOT NULL,
  created_at     INTEGER NOT NULL,
  PRIMARY KEY (sandbox_id, guest_port)
);
INSERT INTO published_new
SELECT sandbox_id, 'default', guest_port, subdomain, visibility, created_at
FROM published;

DROP TABLE published;
DROP TABLE sandboxes;
DROP TABLE snapshots;
DROP TABLE templates;
ALTER TABLE templates_new RENAME TO templates;
ALTER TABLE snapshots_new RENAME TO snapshots;
ALTER TABLE sandboxes_new RENAME TO sandboxes;
ALTER TABLE published_new RENAME TO published;

ALTER TABLE events ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE retired_hostnames ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default';

CREATE INDEX sandboxes_tenant ON sandboxes(tenant_id);
CREATE INDEX snapshots_tenant ON snapshots(tenant_id);
CREATE INDEX events_tenant ON events(tenant_id);
