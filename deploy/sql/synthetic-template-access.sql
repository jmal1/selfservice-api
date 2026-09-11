-- Grants the synthetic monitor user explicit per-user-id access to the
-- synthetic-noop template.
--
-- Why this is needed:
--   The /api/templates handler hides is_internal=true templates from the
--   picker unless the caller has an explicit per-user-id template_access
--   grant for that template. synthetic-noop is is_internal=true (see
--   migration 000019) so without this grant the synthetic UI E2E test
--   (create-and-destroy-pod.spec.ts) can't find the template card when
--   walking /pods/new.
--
-- Run once per environment AFTER:
--   1. selfservice-api image containing migration 000019 has been deployed
--      (so is_internal column exists and synthetic-noop is backfilled to true)
--   2. synthetic-user.sql has been run (so the synthetic user exists)
--
--   PW=$(kubectl -n selfservice get secret selfservice-db-creds \
--          -o jsonpath='{.data.password}' | base64 -d)
--   kubectl -n selfservice cp deploy/sql/synthetic-template-access.sql \
--     selfservice-postgresql-0:/tmp/synthetic-template-access.sql
--   kubectl -n selfservice exec selfservice-postgresql-0 -- \
--     env PGPASSWORD="$PW" psql -U postgres -d selfservice \
--     -f /tmp/synthetic-template-access.sql
--
-- Re-runnable: WHERE NOT EXISTS guard prevents duplicate rows.
INSERT INTO template_access (template_id, user_id, role)
SELECT t.id, u.id, NULL
FROM templates t
CROSS JOIN users u
WHERE t.name = 'synthetic-noop'
  AND u.username = 'synthetic'
  AND NOT EXISTS (
      SELECT 1 FROM template_access ta
      WHERE ta.template_id = t.id AND ta.user_id = u.id
  );
