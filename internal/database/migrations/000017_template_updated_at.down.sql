-- Rollback optimistic locking for templates.
DROP TRIGGER IF EXISTS templates_touch_updated_at ON templates;
-- touch_updated_at() is intentionally kept; it's harmless if no triggers
-- reference it, and may be reused later.
ALTER TABLE templates DROP COLUMN IF EXISTS updated_at;
