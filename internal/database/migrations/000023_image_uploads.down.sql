DROP INDEX IF EXISTS idx_image_uploads_object_key;
DROP INDEX IF EXISTS idx_image_uploads_status;
DROP TABLE IF EXISTS image_uploads;

ALTER TABLE templates DROP CONSTRAINT IF EXISTS templates_unattend_mode_check;
ALTER TABLE templates DROP COLUMN IF EXISTS guest_id;
ALTER TABLE templates DROP COLUMN IF EXISTS unattend_config;
ALTER TABLE templates DROP COLUMN IF EXISTS unattend_mode;
