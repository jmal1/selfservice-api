ALTER TABLE templates
    ADD COLUMN guest_credentials_verified_at TIMESTAMPTZ;

ALTER TABLE pod_vms
    ADD COLUMN guest_credentials_verified_at TIMESTAMPTZ;

ALTER TABLE pod_vms
    ADD COLUMN guest_credentials_verified_vm_id TEXT;
