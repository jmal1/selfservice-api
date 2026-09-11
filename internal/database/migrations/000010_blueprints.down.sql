ALTER TABLE pods DROP COLUMN IF EXISTS allow_vm_additions;
ALTER TABLE pods DROP COLUMN IF EXISTS blueprint_id;
DROP TABLE IF EXISTS blueprint_access;
DROP TABLE IF EXISTS blueprint_vms;
DROP TABLE IF EXISTS blueprints;
