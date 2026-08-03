-- Elevated (instructor-role) synthetic identity for the API monitor.
--
-- WHY A SECOND USER INSTEAD OF RAISING THE FIRST ONE'S ROLE
--
-- Five checks in the catalog assert that the PRIMARY synthetic user is
-- refused: admin_list_users_403, admin_audit_403, wiki_index_rbac,
-- pod_testing_dashboard_404 and image_upload_rbac. Elevating that user would
-- leave all five green while proving nothing at all — the single worst
-- monitoring outcome, because a green board is indistinguishable from a
-- working system. So the elevated identity is a separate row, and the monitor
-- mints a second cookie for it.
--
-- WHY THIS ACCOUNT CANNOT BE LOGGED INTO
--
-- OIDC login resolves users by `oidc_sub` (UNIQUE, see GetUserBySub). The
-- sentinel below is not a shape Authentik can emit, so no human session can
-- ever land on this row. The monitor reaches it only by minting a JWT with
-- the API's shared HMAC — a secret whose holder can already mint any role, so
-- this row grants the monitor no privilege it did not already have.
--
-- WHY THE QUOTAS ARE ZERO
--
-- The elevated checks are strictly read-only (list images, browse the ISO
-- datastore, fetch wizard state for a phantom UUID). Zero pods / vCPUs / RAM
-- means that even a future check with a bug cannot provision anything on this
-- identity, and a leaked cookie cannot be used to consume lab capacity.
--
-- This is NOT a schema migration; it inserts a single data row. Run once per
-- environment:
--
--   PW=$(kubectl -n selfservice get secret selfservice-db-creds \
--          -o jsonpath='{.data.password}' | base64 -d)
--   kubectl -n selfservice cp deploy/sql/synthetic-instructor-user.sql \
--     selfservice-postgresql-0:/tmp/synthetic-instructor-user.sql
--   kubectl -n selfservice exec selfservice-postgresql-0 -- \
--     env PGPASSWORD="$PW" psql -U selfservice -d selfservice \
--     -f /tmp/synthetic-instructor-user.sql
--
-- Then add the id to the EXISTING synthetic secret (do not create a second
-- secret; the CronJob reads both keys from selfservice-synthetic-user):
--
--   IID=$(kubectl -n selfservice exec selfservice-postgresql-0 -- \
--     env PGPASSWORD="$PW" psql -U selfservice -d selfservice -tA \
--     -c "SELECT id FROM users WHERE username='synthetic-instructor'")
--   kubectl -n selfservice patch secret selfservice-synthetic-user \
--     --type merge -p "{\"stringData\":{\"instructor-user-id\":\"$IID\"}}"
--
-- Re-runnable: ON CONFLICT does nothing.
INSERT INTO users (
    oidc_sub,
    username,
    email,
    display_name,
    role,
    max_vcpus,
    max_ram_mb,
    max_pods,
    is_active
) VALUES (
    'synthetic-instructor-no-oidc',         -- sentinel; Authentik cannot produce this sub
    'synthetic-instructor',
    'synthetic-instructor@lab.jmal.io',
    'Synthetic API Monitor (Instructor)',
    'instructor',                           -- least role that reaches the /admin block
    0,                                      -- read-only identity: cannot provision anything
    0,
    0,
    true
) ON CONFLICT (oidc_sub) DO NOTHING;
