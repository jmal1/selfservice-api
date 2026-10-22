-- Sets a fixed Markdown description on the internal synthetic-noop template so
-- the UI synthetics template_description_student_view and
-- template_description_editor (jmal1/Crucible#76) have a known value to assert.
--
-- Why synthetic-noop only:
--   synthetic-noop is is_internal=true (migration 000019): students cannot pick
--   it, so its description is invisible to real students.
--
-- Run once per environment:
--
--   PW=$(kubectl -n selfservice get secret selfservice-db-creds \
--          -o jsonpath='{.data.password}' | base64 -d)
--   kubectl -n selfservice cp deploy/sql/synthetic-template-description.sql \
--     selfservice-postgresql-0:/tmp/synthetic-template-description.sql
--   kubectl -n selfservice exec selfservice-postgresql-0 -- \
--     env PGPASSWORD="$PW" psql -U selfservice -d selfservice \
--     -v ON_ERROR_STOP=1 -f /tmp/synthetic-template-description.sql
--
-- Re-runnable and convergent. It prints the previous description first, so an
-- overwritten value is recoverable from the operator's terminal.

BEGIN;

SELECT name, description AS previous_description
FROM templates
WHERE name = 'synthetic-noop';

UPDATE templates
SET description = $d$**Synthetic fixture description** — internal no-op template used only by synthetic monitors.$d$,
    updated_at = now()
WHERE name = 'synthetic-noop'
  AND description IS DISTINCT FROM $d$**Synthetic fixture description** — internal no-op template used only by synthetic monitors.$d$;

COMMIT;
