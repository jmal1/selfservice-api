-- Migration 004: Add default credentials columns to templates
-- These store the default login credentials for VMs provisioned from each template.

ALTER TABLE templates ADD COLUMN default_username TEXT NOT NULL DEFAULT '';
ALTER TABLE templates ADD COLUMN default_password TEXT NOT NULL DEFAULT '';
