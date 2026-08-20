-- Migration 000032: durable per-check template-health diagnostics.
--
-- The original row shared one counter and error between structural and deep
-- checks. A passing structural check could therefore erase a pending deep
-- failure. Persist each check independently so an independently scheduled
-- deep confirmation survives worker restarts and leader failover.

ALTER TABLE template_health_state ADD COLUMN structural_consecutive_failures INT NOT NULL DEFAULT 0;
ALTER TABLE template_health_state ADD COLUMN deep_consecutive_failures INT NOT NULL DEFAULT 0;
ALTER TABLE template_health_state ADD COLUMN last_structural_passed BOOLEAN;
ALTER TABLE template_health_state ADD COLUMN last_structural_error TEXT;
ALTER TABLE template_health_state ADD COLUMN last_structural_fault_class TEXT;
ALTER TABLE template_health_state ADD COLUMN last_structural_duration_seconds DOUBLE PRECISION;
ALTER TABLE template_health_state ADD COLUMN last_deep_passed BOOLEAN;
ALTER TABLE template_health_state ADD COLUMN last_deep_error TEXT;
ALTER TABLE template_health_state ADD COLUMN last_deep_fault_class TEXT;
ALTER TABLE template_health_state ADD COLUMN last_deep_duration_seconds DOUBLE PRECISION;
ALTER TABLE template_health_state ADD COLUMN pending_deep_failure_at TIMESTAMPTZ;
ALTER TABLE template_health_state ADD COLUMN deep_confirmation_due_at TIMESTAMPTZ;

-- Migration 000031 defined the legacy counter as structural-check progress.
-- Preserve that anti-flap state so a row already at one failure still becomes
-- unhealthy on its next failed structural cycle after rollout.
UPDATE template_health_state
SET structural_consecutive_failures = consecutive_failures;

CREATE TABLE template_health_reconcile_state (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    last_completed_at TIMESTAMPTZ
);

CREATE INDEX idx_template_health_pending_deep_confirmation
    ON template_health_state (deep_confirmation_due_at)
    WHERE pending_deep_failure_at IS NOT NULL;
