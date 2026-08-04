-- Add visibility column to templates so instructors can stage templates
-- without exposing them to students.
--
-- visibility defaults to 'public' to preserve existing behavior.
-- Students see only 'public' templates (composed with is_internal).
-- Instructors and admins see all templates regardless of visibility.
--
-- This is distinct from is_internal (migration 000019), which hides
-- infrastructure/fixture templates. Visibility is for instructor-only
-- staging of real content.

ALTER TABLE templates
    ADD COLUMN visibility TEXT NOT NULL DEFAULT 'public';

ALTER TABLE templates
    ADD CONSTRAINT templates_visibility_check
    CHECK (visibility IN ('public', 'instructor_only'));

-- Index to support the student-visible query:
-- WHERE visibility = 'public' AND is_active = true AND ...
CREATE INDEX idx_templates_visibility_active ON templates(visibility, is_active);
