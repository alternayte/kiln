-- 0007: registry credentials. A template build pulls a private image with the
-- credentials of its own tenant, never the operator's, so one tenant's token
-- can never pull for another. The token is sealed with the host key before it
-- reaches this table, because kiln.db is read by more than the operator.
-- An applied migration is never edited.

CREATE TABLE registries (
  tenant_id  TEXT NOT NULL REFERENCES tenants(id),
  host       TEXT NOT NULL,
  username   TEXT NOT NULL,
  token      TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, host)
);

-- A preview template is made by a machine and forgotten by a human, so the
-- host ends one with no client involved.
ALTER TABLE templates ADD COLUMN ttl_seconds INTEGER;
