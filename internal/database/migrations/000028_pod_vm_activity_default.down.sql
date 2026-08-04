-- Reverse 000028. The backfilled timestamps are deliberately NOT reverted to
-- NULL: doing so would restore the "infinitely idle" reading that made
-- auto-suspend unsafe, and a down migration should not reintroduce a hazard.
ALTER TABLE pod_vms ALTER COLUMN last_activity_at DROP DEFAULT;
