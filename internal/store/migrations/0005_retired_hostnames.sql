-- 0005: retired preview hostnames. A retired hostname 404s forever and is
-- never handed out again, so every one leaves a tombstone. The published row
-- goes; the tombstone stays. An applied migration is never edited.

CREATE TABLE retired_hostnames (
  subdomain      TEXT PRIMARY KEY,
  retired_at     INTEGER NOT NULL
);
