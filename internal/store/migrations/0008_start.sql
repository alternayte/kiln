-- 0008: the start command. Kiln boots kilninit as PID 1 and ignores what the
-- image says to run, so a template that serves an application names the
-- command and the port itself. An applied migration is never edited.

ALTER TABLE templates ADD COLUMN start_cmd TEXT;
ALTER TABLE templates ADD COLUMN start_port INTEGER;
