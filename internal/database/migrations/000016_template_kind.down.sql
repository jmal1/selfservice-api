-- Rollback of 000016_template_kind.
--
-- Safe even if some rows were inserted with kind != 'clone_with_customize':
-- the data is dropped along with the columns, but the deployment must have
-- already redeployed code that doesn't reference these columns first.

DROP INDEX IF EXISTS idx_templates_kind;
ALTER TABLE templates DROP COLUMN IF EXISTS assign_ip;
ALTER TABLE templates DROP COLUMN IF EXISTS kind;
