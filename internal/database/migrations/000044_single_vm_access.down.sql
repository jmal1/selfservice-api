BEGIN;

ALTER TABLE templates DROP COLUMN IF EXISTS single_vm_only;

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_max_single_vms_range;
ALTER TABLE users DROP COLUMN IF EXISTS max_single_vms;
ALTER TABLE users DROP COLUMN IF EXISTS labs_enabled;

COMMIT;
