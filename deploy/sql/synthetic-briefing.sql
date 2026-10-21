-- Publishes a fixed lab briefing on the internal synthetic-noop template so the
-- synthetic student's fixture pod (ui-runner-results-fixture) shows the student
-- briefing view. The UI synthetic briefing_student_view asserts on its heading.
--
-- Why synthetic-noop only:
--   synthetic-noop is is_internal=true (migration 000019): students cannot pick
--   it, so only synthetic accounts ever have pods from it. A briefing here is
--   invisible to real students.
--
-- Run once per environment AFTER migration 000047 (briefings) is applied:
--
--   PW=$(kubectl -n selfservice get secret selfservice-db-creds \
--          -o jsonpath='{.data.password}' | base64 -d)
--   kubectl -n selfservice cp deploy/sql/synthetic-briefing.sql \
--     selfservice-postgresql-0:/tmp/synthetic-briefing.sql
--   kubectl -n selfservice exec selfservice-postgresql-0 -- \
--     env PGPASSWORD="$PW" psql -U selfservice -d selfservice \
--     -v ON_ERROR_STOP=1 -f /tmp/synthetic-briefing.sql
--
-- Re-runnable and convergent: it appends a revision only when the published
-- body differs from the one below, and always publishes that body.

BEGIN;

CREATE TEMP TABLE fixture ON COMMIT DROP AS
SELECT t.id AS template_id, $body$# Synthetic fixture briefing

This briefing exists only for synthetic monitoring (`briefing_student_view`).
It is published on the internal `synthetic-noop` template, which students cannot use.

```bash
echo crucible-briefing-fixture
```
$body$::text AS body
FROM templates t
WHERE t.name = 'synthetic-noop';

INSERT INTO briefings (template_id)
SELECT template_id FROM fixture
ON CONFLICT DO NOTHING;

INSERT INTO briefing_revisions (briefing_id, revision, parent_id, body_md, body_sha256, message)
SELECT b.id,
       COALESCE((SELECT max(r.revision) FROM briefing_revisions r WHERE r.briefing_id = b.id), 0) + 1,
       (SELECT r.id FROM briefing_revisions r WHERE r.briefing_id = b.id ORDER BY r.revision DESC LIMIT 1),
       f.body,
       encode(sha256(convert_to(f.body, 'UTF8')), 'hex'),
       'synthetic monitoring fixture (deploy/sql/synthetic-briefing.sql)'
FROM fixture f
JOIN briefings b ON b.template_id = f.template_id
WHERE NOT EXISTS (
    SELECT 1 FROM briefing_revisions r WHERE r.briefing_id = b.id AND r.body_md = f.body
);

UPDATE briefings b
SET published_revision_id = (
        SELECT r.id FROM briefing_revisions r
        WHERE r.briefing_id = b.id AND r.body_md = f.body
        ORDER BY r.revision DESC LIMIT 1),
    updated_at = now()
FROM fixture f
WHERE b.template_id = f.template_id;

COMMIT;
