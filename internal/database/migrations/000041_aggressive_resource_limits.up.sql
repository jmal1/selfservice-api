-- Aggressive resource-conservation policy. Quota and expiry clamps are
-- intentionally one-way: deleted capacity must not be silently restored.

ALTER TABLE suspend_settings
    ALTER COLUMN idle_timeout_seconds SET DEFAULT 7200;

ALTER TABLE pods
    ADD COLUMN destroy_retry_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN destroy_retry_after TIMESTAMPTZ;

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

-- Re-seed the deliberately smaller deploy/sql bootstrap identities after the
-- role-wide clamps. These values mirror those files exactly.
INSERT INTO users (oidc_sub, username, email, display_name, role, max_vcpus, max_ram_mb, max_pods, is_active)
VALUES ('synthetic-monitor-no-oidc', 'synthetic', 'synthetic@example.com', 'Synthetic API Monitor', 'student', 1, 512, 1, true)
ON CONFLICT (oidc_sub) DO UPDATE
SET username = EXCLUDED.username,
    email = EXCLUDED.email,
    display_name = EXCLUDED.display_name,
    role = EXCLUDED.role,
    max_vcpus = EXCLUDED.max_vcpus,
    max_ram_mb = EXCLUDED.max_ram_mb,
    max_pods = EXCLUDED.max_pods,
    is_active = EXCLUDED.is_active,
    updated_at = now();

INSERT INTO users (oidc_sub, username, email, display_name, role, max_vcpus, max_ram_mb, max_pods, is_active)
VALUES ('synthetic-instructor-no-oidc', 'synthetic-instructor', 'synthetic-instructor@example.com', 'Synthetic API Monitor (Instructor)', 'instructor', 0, 0, 0, true)
ON CONFLICT (oidc_sub) DO UPDATE
SET username = EXCLUDED.username,
    email = EXCLUDED.email,
    display_name = EXCLUDED.display_name,
    role = EXCLUDED.role,
    max_vcpus = EXCLUDED.max_vcpus,
    max_ram_mb = EXCLUDED.max_ram_mb,
    max_pods = EXCLUDED.max_pods,
    is_active = EXCLUDED.is_active,
    updated_at = now();

UPDATE pods p
SET expires_at = LEAST(
        COALESCE(p.expires_at, now() + CASE WHEN u.role IN ('instructor', 'admin') THEN interval '14 days' ELSE interval '5 days' END),
        now() + CASE WHEN u.role IN ('instructor', 'admin') THEN interval '14 days' ELSE interval '5 days' END
    ),
    updated_at = now()
FROM users u
WHERE u.id = p.owner_id
  AND p.status <> 'destroyed';
