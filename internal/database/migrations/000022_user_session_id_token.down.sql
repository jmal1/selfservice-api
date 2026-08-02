-- Rollback per-session id_token persistence.
ALTER TABLE user_sessions DROP COLUMN IF EXISTS id_token;
