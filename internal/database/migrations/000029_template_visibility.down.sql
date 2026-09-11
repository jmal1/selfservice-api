-- Reverse 000029_template_visibility.up.sql.

DROP INDEX IF EXISTS idx_templates_visibility_active;
ALTER TABLE templates DROP CONSTRAINT IF EXISTS templates_visibility_check;
ALTER TABLE templates DROP COLUMN IF EXISTS visibility;
