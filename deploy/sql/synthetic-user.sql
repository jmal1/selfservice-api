-- Synthetic user bootstrap for selfservice-synthetic-api-monitor.
--
-- This is NOT a schema migration (no ALTER TABLE); it inserts a single data
-- row. Run once per environment when standing up the synthetic monitor:
--
--   PW=$(kubectl -n selfservice get secret selfservice-db-creds \
--          -o jsonpath='{.data.password}' | base64 -d)
--   kubectl -n selfservice cp deploy/sql/synthetic-user.sql \
--     selfservice-postgresql-0:/tmp/synthetic-user.sql
--   kubectl -n selfservice exec selfservice-postgresql-0 -- \
--     env PGPASSWORD="$PW" psql -U postgres -d selfservice \
--     -f /tmp/synthetic-user.sql
--
-- Then capture the inserted id and create the K8s secret:
--
--   USER_ID=$(kubectl -n selfservice exec selfservice-postgresql-0 -- \
--     env PGPASSWORD="$PW" psql -U postgres -d selfservice -tA \
--     -c "SELECT id FROM users WHERE username='synthetic'")
--   kubectl -n selfservice create secret generic selfservice-synthetic-user \
--     --from-literal=user-id="$USER_ID"
--
-- Re-runnable: ON CONFLICT repairs quotas if a migration or operator lowered
-- them. The synthetic-noop template requires at least 1 vCPU / 1024 MiB; these
-- values match the production recovery and leave room for the lifecycle
-- check's create/cleanup path.
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
    'synthetic-monitor-no-oidc',           -- fake sub; synthetic auth bypasses Authentik
    'synthetic',
    'synthetic@example.com',
    'Synthetic API Monitor',
    'student',                              -- intentionally low privilege; admin_403 check relies on this
    3,
    3072,
    3,
    true
) ON CONFLICT DO UPDATE SET
    max_vcpus = GREATEST(users.max_vcpus, EXCLUDED.max_vcpus),
    max_ram_mb = GREATEST(users.max_ram_mb, EXCLUDED.max_ram_mb),
    max_pods = GREATEST(users.max_pods, EXCLUDED.max_pods),
    updated_at = now();
