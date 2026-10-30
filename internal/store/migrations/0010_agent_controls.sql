-- 0010: what a template runs as, the egress policy of one sandbox and of one
-- snapshot, and the timing of each phase of a build or a restore. A NULL
-- egress_allow means the policy of the template. An applied migration is
-- never edited.

ALTER TABLE templates ADD COLUMN run_as TEXT;
ALTER TABLE sandboxes ADD COLUMN egress_allow TEXT;
ALTER TABLE snapshots ADD COLUMN egress_allow TEXT;
ALTER TABLE events ADD COLUMN template_name TEXT;
ALTER TABLE events ADD COLUMN phase TEXT;
ALTER TABLE events ADD COLUMN duration_ms INTEGER;
