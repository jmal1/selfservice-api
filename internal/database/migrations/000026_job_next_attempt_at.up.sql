-- Migration 000026: add next_attempt_at to jobs for retry scheduling.
--
-- When a job fails with a retryable error (e.g. vCenter's intermittent
-- "virtual disk is either corrupted or not a supported format" clone
-- rejection), the worker sets next_attempt_at to a future time with
-- exponential backoff + jitter and resets status to 'pending'.  ClaimJob
-- skips rows whose next_attempt_at is still in the future, so the job is
-- invisible to workers until the delay expires.
--
-- NULL means "ready immediately" (all pre-existing pending rows, and any
-- new job that has never failed).  The partial index below keeps
-- ClaimJob fast: it only indexes the small set of rows that are actively
-- sleeping.

ALTER TABLE jobs ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_jobs_retry_scheduled
    ON jobs(next_attempt_at)
    WHERE status = 'pending' AND next_attempt_at IS NOT NULL;
