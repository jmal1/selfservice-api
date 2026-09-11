-- Migration 000028: give every pod VM an honest idle clock from birth.
--
-- last_activity_at arrived in 000025 with no DEFAULT and no writer on the
-- INSERT path, so every newly-provisioned VM started life as NULL. The idle
-- evaluator treats NULL as "no activity ever recorded", i.e. INFINITELY idle —
-- so a VM created seconds ago was immediately eligible for suspension the first
-- time its CPU happened to look quiet, before a student ever connected to it.
--
-- Backfilling alone is not enough: without a DEFAULT, every future INSERT
-- recreates the bug. Both halves are required.
--
-- The evaluator's candidate query additionally COALESCEs to created_at, so an
-- insert path that somehow bypasses this default still cannot produce a VM that
-- looks infinitely idle. Defence in depth, because the failure mode here is
-- destroying a student's running work.

ALTER TABLE pod_vms ALTER COLUMN last_activity_at SET DEFAULT now();

-- Existing rows: fall back to created_at, which is the earliest moment the VM
-- could plausibly have been used. This can only ever DELAY a suspension
-- relative to NULL, never cause one.
UPDATE pod_vms
SET last_activity_at = COALESCE(created_at, now())
WHERE last_activity_at IS NULL;
