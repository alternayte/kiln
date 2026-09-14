-- 0004: published previews. The subdomain holds 128 bits from a CSPRNG and
-- is unique for the life of the database. A retired hostname is deleted; it
-- 404s from then on and is never handed out again. An applied migration is
-- never edited.

CREATE TABLE published (
  sandbox_id     TEXT NOT NULL REFERENCES sandboxes(id),
  guest_port     INTEGER NOT NULL,
  subdomain      TEXT NOT NULL UNIQUE,
  visibility     TEXT NOT NULL,
  created_at     INTEGER NOT NULL,
  PRIMARY KEY (sandbox_id, guest_port)
);
