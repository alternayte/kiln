-- 0011: the services of a template. Their env never reaches this row, because
-- a value there would reach every read of the template. An applied migration
-- is never edited.

ALTER TABLE templates ADD COLUMN services TEXT;
