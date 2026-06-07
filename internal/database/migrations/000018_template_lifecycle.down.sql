-- Rollback for template lifecycle columns.
-- Safe to apply on a live database: no data loss for non-wizard templates
-- (all the columns being dropped were defaulted on existing rows).
-- Wizard-created templates that hadn't reached 'active' state yet would
-- lose their lifecycle tracking, but the VMs themselves would remain
-- intact in vCenter (just unreachable via the wizard).

ALTER TABLE templates DROP CONSTRAINT IF EXISTS templates_source_type_check;
ALTER TABLE templates DROP CONSTRAINT IF EXISTS templates_state_check;
DROP INDEX IF EXISTS idx_templates_created_by;
DROP INDEX IF EXISTS idx_templates_state;
ALTER TABLE templates DROP COLUMN IF EXISTS staging_network;
ALTER TABLE templates DROP COLUMN IF EXISTS source_ref;
ALTER TABLE templates DROP COLUMN IF EXISTS source_type;
ALTER TABLE templates DROP COLUMN IF EXISTS vcenter_vm_id;
ALTER TABLE templates DROP COLUMN IF EXISTS created_by;
ALTER TABLE templates DROP COLUMN IF EXISTS template_state;
