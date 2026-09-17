-- 0009: the key names of the env a sandbox was created with. The values are
-- secrets and never reach this file; the names let a caller see which keys a
-- sandbox holds. An applied migration is never edited.

ALTER TABLE sandboxes ADD COLUMN env_keys TEXT NOT NULL DEFAULT '[]';
