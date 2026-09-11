DROP INDEX IF EXISTS idx_actions_platforms;
ALTER TABLE actions DROP COLUMN IF EXISTS supported_platforms;
