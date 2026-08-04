-- Migration 000031: per-template health check state.
--
-- MERGE ORDER: This migration MUST be applied AFTER migrations 000029
-- and 000030. See PR body for details.
--
-- The template health reconciler runs every 12 hours and tracks two
-- check types per template:
--
--   structural — cheap, read-only vCenter metadata assertions: does the
--                referenced VM/template object still exist?
--   deep       — one real clone → power-on → wait for IP → destroy per
--                cycle, round-robin across all student-visible templates.
--
-- Anti-flap guarantees:
--   • Each failing check is retried 3× with exponential backoff before
--     recording a failure (protects against transient vCenter errors).
--   • A template is only marked unhealthy after 2 consecutive failed
--     cycles — a single bad 12-hour window never alerts.
--   • Recovery is immediate: one passing cycle resets consecutive_failures
--     to 0 and sets health_status = 'healthy'.
--   • Checker-level failures (vCenter unreachable, auth expired) set
--     crucible_template_health_checker_up = 0 and do NOT mutate per-template
--     rows — so a vCenter outage fires exactly one infrastructure alert,
--     not N template alerts.

CREATE TABLE IF NOT EXISTS template_health_state (
    template_id             UUID        NOT NULL PRIMARY KEY
                                        REFERENCES templates(id) ON DELETE CASCADE,
    -- 'healthy' | 'unhealthy' | 'unknown'
    -- 'unknown' is the initial state before any check has completed.
    health_status           TEXT        NOT NULL DEFAULT 'unknown'
                                        CHECK (health_status IN ('healthy', 'unhealthy', 'unknown')),
    -- Number of consecutive cycles in which the structural check failed
    -- after all retries were exhausted. Resets to 0 on any passing cycle.
    -- Used to implement the 2-cycle confirmation rule.
    consecutive_failures    INT         NOT NULL DEFAULT 0,
    last_structural_check_at TIMESTAMPTZ,
    last_deep_check_at      TIMESTAMPTZ,
    -- Short human-readable error from the most recent failing check, or
    -- NULL when the last check passed. Visible to instructors in the UI.
    last_error              TEXT,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Partial index: fast lookup of templates with consecutive failures (alert
-- candidate query run by the reconciler after each cycle).
CREATE INDEX IF NOT EXISTS idx_template_health_state_unhealthy
    ON template_health_state (template_id)
    WHERE health_status = 'unhealthy';
