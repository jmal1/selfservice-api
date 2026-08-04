-- Rollback: remove pinning columns and indexes from templates and blueprints

DROP INDEX IF EXISTS idx_blueprints_pinned_order;
DROP INDEX IF EXISTS idx_templates_pinned_order;

ALTER TABLE blueprints
    DROP COLUMN IF EXISTS pinned_by,
    DROP COLUMN IF EXISTS pinned_at,
    DROP COLUMN IF EXISTS pin_order,
    DROP COLUMN IF EXISTS pinned;

ALTER TABLE templates
    DROP COLUMN IF EXISTS pinned_by,
    DROP COLUMN IF EXISTS pinned_at,
    DROP COLUMN IF EXISTS pin_order,
    DROP COLUMN IF EXISTS pinned;
