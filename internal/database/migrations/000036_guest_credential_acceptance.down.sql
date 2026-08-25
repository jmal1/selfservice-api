ALTER TABLE pod_vms
    DROP COLUMN IF EXISTS guest_credentials_verified_vm_id;

ALTER TABLE pod_vms
    DROP COLUMN IF EXISTS guest_credentials_verified_at;

ALTER TABLE templates
    DROP COLUMN IF EXISTS guest_credentials_verified_at;
