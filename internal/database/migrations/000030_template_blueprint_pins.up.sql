-- Add pinning support for templates and blueprints.
-- Instructors can pin items to emphasize them in the list, making them
-- appear in a dedicated "Pinned" section above the normal list.
-- Pins are explicitly ordered so instructors control the display sequence.
--
-- pinned: boolean flag to indicate if an item is pinned
-- pin_order: explicit ordering for pinned items (lower numbers first)
-- pinned_at: timestamp when the item was pinned (for use as tiebreaker)
--
-- Sorting order: pin_order ASC, pinned_at DESC NULLS LAST, name ASC
-- The name tiebreak ensures a total order (prevents shuffle on page reload).
-- The partial index optimizes queries that filter on (pinned, pin_order).

ALTER TABLE templates ADD COLUMN pinned BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE templates ADD COLUMN pin_order INTEGER NOT NULL DEFAULT 0;
ALTER TABLE templates ADD COLUMN pinned_at TIMESTAMPTZ;

ALTER TABLE blueprints ADD COLUMN pinned BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE blueprints ADD COLUMN pin_order INTEGER NOT NULL DEFAULT 0;
ALTER TABLE blueprints ADD COLUMN pinned_at TIMESTAMPTZ;

-- Partial indexes for queries that list pinned items
CREATE INDEX idx_templates_pinned_order ON templates (pinned, pin_order) WHERE pinned;
CREATE INDEX idx_blueprints_pinned_order ON blueprints (pinned, pin_order) WHERE pinned;
