-- Migration 005: Add generated credentials columns to pod_vms
-- Each VM gets a unique password injected via cloud-init at provisioning time.

ALTER TABLE pod_vms ADD COLUMN IF NOT EXISTS generated_username TEXT NOT NULL DEFAULT '';
ALTER TABLE pod_vms ADD COLUMN IF NOT EXISTS generated_password TEXT NOT NULL DEFAULT '';
