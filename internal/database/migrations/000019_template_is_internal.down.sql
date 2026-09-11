-- Reverse 000019_template_is_internal.up.sql.

ALTER TABLE templates DROP COLUMN IF EXISTS is_internal;
