-- Rollback: remove default credentials columns from templates

ALTER TABLE templates DROP COLUMN IF EXISTS default_username;
ALTER TABLE templates DROP COLUMN IF EXISTS default_password;
