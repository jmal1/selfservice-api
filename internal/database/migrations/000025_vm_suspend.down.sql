DROP TABLE IF EXISTS suspend_settings;

ALTER TABLE pod_vms
    DROP COLUMN IF EXISTS suspend_reason,
    DROP COLUMN IF EXISTS suspended_at,
    DROP COLUMN IF EXISTS last_activity_at,
    DROP COLUMN IF EXISTS last_console_at;
