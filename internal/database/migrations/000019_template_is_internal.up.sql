-- Add is_internal flag to templates so the synthetic monitor's
-- "synthetic-noop" template (and any future synthetic / fixture
-- templates) can be hidden from the user-facing template picker
-- without losing the existing access-control rules.
--
-- Before this migration synthetic-noop was visible to every user with
-- only "[INTERNAL]" in its description as a soft deterrent. After this
-- migration the API filters is_internal=true rows from the public
-- ListTemplates endpoint while keeping them resolvable by ID from the
-- pod-create / vm-add server-side handlers (so the synthetic user can
-- still create pods from it through the API).
--
-- The admin-only /admin/templates list (ListAllTemplates) continues
-- to show internal templates for visibility and management.

ALTER TABLE templates
    ADD COLUMN IF NOT EXISTS is_internal BOOLEAN NOT NULL DEFAULT FALSE;

-- Backfill: the only existing internal template is synthetic-noop,
-- which the synthetic monitor uses for its lifecycle check. Tagging
-- by name is safe because the unique-by-name invariant has held
-- since migration 000001.
UPDATE templates
SET is_internal = TRUE
WHERE name = 'synthetic-noop';
