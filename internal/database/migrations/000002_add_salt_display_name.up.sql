-- Add salt column to pods for generating unique vcenter VM names
ALTER TABLE pods ADD COLUMN IF NOT EXISTS salt VARCHAR(6) NOT NULL DEFAULT '';

-- Add display_name column to pod_vms for user-chosen VM names
ALTER TABLE pod_vms ADD COLUMN IF NOT EXISTS display_name VARCHAR(64) NOT NULL DEFAULT '';

-- Backfill salt for existing pods using random hex
UPDATE pods SET salt = substr(md5(random()::text), 1, 6) WHERE salt = '';

-- Backfill display_name from vcenter_vm_name for existing VMs
UPDATE pod_vms SET display_name = COALESCE(vcenter_vm_name, 'VM') WHERE display_name = '';
