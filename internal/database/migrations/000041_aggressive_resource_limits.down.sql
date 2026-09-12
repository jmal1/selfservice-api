-- Quota and expires_at clamps are irreversible by design. Only restore the
-- suspend_settings schema default; existing global policy remains explicit.
ALTER TABLE suspend_settings
    ALTER COLUMN idle_timeout_seconds SET DEFAULT 21600;

ALTER TABLE pods
    DROP COLUMN IF EXISTS destroy_retry_after,
    DROP COLUMN IF EXISTS destroy_retry_count;
