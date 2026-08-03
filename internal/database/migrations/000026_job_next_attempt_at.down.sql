DROP INDEX IF EXISTS idx_jobs_retry_scheduled;
ALTER TABLE jobs DROP COLUMN IF EXISTS next_attempt_at;
