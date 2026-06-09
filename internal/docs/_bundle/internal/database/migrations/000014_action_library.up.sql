-- Make workflow_id nullable so actions can be standalone library entries
ALTER TABLE actions ALTER COLUMN workflow_id DROP NOT NULL;

-- Add new columns for library actions
ALTER TABLE actions ADD COLUMN script TEXT NOT NULL DEFAULT '';
ALTER TABLE actions ADD COLUMN input_context JSONB NOT NULL DEFAULT '[]';
ALTER TABLE actions ADD COLUMN output_context JSONB NOT NULL DEFAULT '[]';
ALTER TABLE actions ADD COLUMN is_library BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE actions ADD COLUMN action_category TEXT NOT NULL DEFAULT 'general';
ALTER TABLE actions ADD COLUMN slug TEXT;
ALTER TABLE actions ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- Index for library actions
CREATE INDEX idx_actions_library ON actions(is_library, action_category) WHERE is_library = true;
CREATE UNIQUE INDEX idx_actions_slug ON actions(slug) WHERE slug IS NOT NULL;
