-- Optimistic locking for templates.
--
-- Adds an updated_at column to templates plus a trigger that keeps it
-- automatically in sync on every UPDATE. Backfills existing rows from
-- created_at so the column is NOT NULL with no missing values.
--
-- The application layer (Queries.UpdateTemplate) uses this column to
-- detect concurrent admin edits and return HTTP 409 to the second
-- writer, preventing silent overwrites of the first writer's changes.

ALTER TABLE templates
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Backfill any rows where the default didn't apply (existing rows on
-- live databases get the DEFAULT, but be explicit for safety).
UPDATE templates SET updated_at = COALESCE(updated_at, created_at, now())
WHERE updated_at IS NULL;

-- Auto-bump updated_at on every UPDATE so the application never has to
-- remember to set it. Use a trigger function shared across tables in
-- case we extend optimistic locking to other resources later.
CREATE OR REPLACE FUNCTION touch_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS templates_touch_updated_at ON templates;
CREATE TRIGGER templates_touch_updated_at
    BEFORE UPDATE ON templates
    FOR EACH ROW
    EXECUTE FUNCTION touch_updated_at();
