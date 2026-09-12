-- Aggressive resource-conservation policy. Quota and expiry clamps are
-- intentionally one-way: deleted capacity must not be silently restored.

ALTER TABLE suspend_settings
    ALTER COLUMN idle_timeout_seconds SET DEFAULT 7200;

ALTER TABLE pods
    ADD COLUMN IF NOT EXISTS destroy_retry_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS destroy_retry_after TIMESTAMPTZ;

UPDATE suspend_settings
SET idle_timeout_seconds = 7200
WHERE scope = 'global' AND scope_id IS NULL;

INSERT INTO suspend_settings (scope, scope_id, idle_timeout_seconds)
SELECT 'global', NULL, 7200
WHERE NOT EXISTS (
    SELECT 1 FROM suspend_settings WHERE scope = 'global' AND scope_id IS NULL
);

UPDATE users
SET max_pods = LEAST(max_pods, 2),
    max_vcpus = LEAST(max_vcpus, 4),
    max_ram_mb = LEAST(max_ram_mb, 8192),
    updated_at = now()
WHERE role = 'student';

UPDATE users
SET max_pods = LEAST(max_pods, 3),
    max_vcpus = LEAST(max_vcpus, 8),
    max_ram_mb = LEAST(max_ram_mb, 16384),
    updated_at = now()
WHERE role = 'instructor';

-- Restore deliberately smaller deploy/sql bootstrap quotas after role-wide
-- clamps. UPDATE-only: live rows may already own username "synthetic" under a
-- different oidc_sub than the bootstrap sentinel, so INSERT ... ON CONFLICT
-- (oidc_sub) collides on users_username_key.
UPDATE users
SET max_vcpus = 1,
    max_ram_mb = 512,
    max_pods = 1,
    updated_at = now()
WHERE oidc_sub = 'synthetic-monitor-no-oidc'
   OR username = 'synthetic';

UPDATE users
SET max_vcpus = 0,
    max_ram_mb = 0,
    max_pods = 0,
    updated_at = now()
WHERE oidc_sub = 'synthetic-instructor-no-oidc'
   OR username = 'synthetic-instructor';

UPDATE pods p
SET expires_at = LEAST(
        COALESCE(p.expires_at, now() + CASE WHEN u.role IN ('instructor', 'admin') THEN interval '14 days' ELSE interval '5 days' END),
        now() + CASE WHEN u.role IN ('instructor', 'admin') THEN interval '14 days' ELSE interval '5 days' END
    ),
    updated_at = now()
FROM users u
WHERE u.id = p.owner_id
  AND p.status <> 'destroyed';
