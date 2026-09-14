-- 0003: snapshots become real in P4. listed marks a snapshot the operator
-- created and can see; a fork writes an unlisted image that lives while its
-- children do. secret_bearing marks a memory image that holds injected
-- secrets, so fork and restore can refuse it by default. origin_sandbox_id
-- names the sandbox whose memory the image holds; a restore inherits the
-- lifecycle fields from it. An applied migration is never edited.

ALTER TABLE sandboxes ADD COLUMN secret_bearing INTEGER NOT NULL DEFAULT 0;

ALTER TABLE snapshots ADD COLUMN secret_bearing INTEGER NOT NULL DEFAULT 0;
ALTER TABLE snapshots ADD COLUMN listed INTEGER NOT NULL DEFAULT 1;
ALTER TABLE snapshots ADD COLUMN origin_sandbox_id TEXT;
