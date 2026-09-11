-- VM idle-suspend: activity timestamps, suspension state, and suspend settings.
--
-- last_console_at   — set by the API layer whenever a WebMKS console session
--                     opens, closes, or sends a periodic heartbeat. Captures
--                     the most recent human-facing console interaction.
--
-- last_activity_at  — the unified freshness signal used by the idle evaluator.
--                     Updated by: console open/heartbeat/close, AND by the
--                     evaluator itself whenever it observes above-threshold
--                     CPU or network utilisation. A VM is a suspension candidate
--                     only when this timestamp is older than the idle threshold.
--
-- suspended_at      — set when a vm_suspend job completes successfully. Cleared
--                     on next power-on. Retained in destroyed rows for audit.
--
-- suspend_reason    — human-readable reason logged with the suspension event
--                     (e.g. "idle: no console activity and low CPU/net for 6h").

ALTER TABLE pod_vms ADD COLUMN IF NOT EXISTS last_console_at  TIMESTAMPTZ;
ALTER TABLE pod_vms ADD COLUMN IF NOT EXISTS last_activity_at TIMESTAMPTZ;
ALTER TABLE pod_vms ADD COLUMN IF NOT EXISTS suspended_at     TIMESTAMPTZ;
ALTER TABLE pod_vms ADD COLUMN IF NOT EXISTS suspend_reason   TEXT;

-- suspend_settings holds the global idle-timeout default and optional per-pod
-- overrides. The idle evaluator resolves: per-pod row if one exists, else the
-- global row, else the compile-time default of 21600 s (6 hours).
--
-- Schema choice: a single table keyed by (scope, scope_id) is the simplest
-- approach that handles "global default + per-pod override" without requiring
-- an ALTER TABLE pods. The cardinality is small (one global row + at most one
-- row per pod that needs a custom timeout), and a UNIQUE constraint on
-- (scope, scope_id) makes the COALESCE lookup safe.
CREATE TABLE IF NOT EXISTS suspend_settings (
    id                   UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    scope                TEXT        NOT NULL CHECK (scope IN ('global', 'pod')),
    scope_id             UUID,                -- NULL for scope='global', pod.id for scope='pod'
    idle_timeout_seconds INTEGER     NOT NULL DEFAULT 21600,  -- default 6 hours
    UNIQUE (scope, scope_id)
);

-- Seed the global default. ON CONFLICT DO NOTHING so re-running the migration
-- (or an out-of-order apply) is idempotent.
INSERT INTO suspend_settings (scope, scope_id, idle_timeout_seconds)
VALUES ('global', NULL, 21600)
ON CONFLICT (scope, scope_id) DO NOTHING;
