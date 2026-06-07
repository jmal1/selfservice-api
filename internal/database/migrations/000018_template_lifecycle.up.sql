-- Template lifecycle state machine — Phase 0 of T4 (template creation wizard).
--
-- Adds six columns that drive the instructor-driven template creation
-- flow: draft → provisioning → configuring → generalizing → ready → active.
--
-- Backwards compatible: every existing template gets template_state='active'
-- + source_type='manual' (the only reasonable default for already-published
-- templates that were created via direct SQL or the v1 admin form). No
-- production handler reads these columns yet, so applying the migration is
-- a no-op for live traffic.
--
-- Related design: future/Template-Creation-Workflow.md in the homelab vault.

ALTER TABLE templates
    ADD COLUMN IF NOT EXISTS template_state TEXT NOT NULL DEFAULT 'active';

-- Creator tracking. NULL allowed so legacy rows (created before the wizard)
-- don't violate the constraint. The wizard will always populate this.
ALTER TABLE templates
    ADD COLUMN IF NOT EXISTS created_by UUID REFERENCES users(id);

-- vCenter VM Managed Object Reference (MoRef) for the template VM while it's
-- being authored. Empty for finished templates (they're identified by name
-- via vcenter_template). Populated during provisioning, cleared on cancel.
ALTER TABLE templates
    ADD COLUMN IF NOT EXISTS vcenter_vm_id TEXT NOT NULL DEFAULT '';

-- How the template was authored. 'manual' is the legacy path (pre-wizard,
-- VM created in vCenter directly and registered via admin form).
--   'clone_template'   - cloned from a published library template
--   'clone_vcenter'    - cloned from an arbitrary existing vCenter VM
--   'iso'              - created from an ISO via wizard's clean-install path
ALTER TABLE templates
    ADD COLUMN IF NOT EXISTS source_type TEXT NOT NULL DEFAULT 'manual';

-- Source identifier: MoRef of source template/VM, or ISO datastore path.
-- Empty for source_type='manual' (no machine-readable source).
ALTER TABLE templates
    ADD COLUMN IF NOT EXISTS source_ref TEXT NOT NULL DEFAULT '';

-- Network the template VM is attached to during the configuring phase.
-- Defaults to LabVMs-VLAN30 (the existing VLAN 30 port group present on
-- every ESXi host, with DHCP from OPNsense and internet access for
-- installer downloads). Authors can override per template if they need
-- to test against a particular pod-style network.
ALTER TABLE templates
    ADD COLUMN IF NOT EXISTS staging_network TEXT NOT NULL DEFAULT 'LabVMs-VLAN30';

-- Constrain template_state to the documented enum. Anything else means a
-- corrupted row and we want a hard error, not silent acceptance.
ALTER TABLE templates
    DROP CONSTRAINT IF EXISTS templates_state_check;
ALTER TABLE templates
    ADD CONSTRAINT templates_state_check
    CHECK (template_state IN ('draft', 'provisioning', 'configuring',
                              'generalizing', 'ready', 'active', 'error'));

-- Constrain source_type similarly. Future authoring paths must be added here.
ALTER TABLE templates
    DROP CONSTRAINT IF EXISTS templates_source_type_check;
ALTER TABLE templates
    ADD CONSTRAINT templates_source_type_check
    CHECK (source_type IN ('manual', 'clone_template', 'clone_vcenter', 'iso'));

-- Filtering templates by state is the dominant query the wizard runs
-- ("show me my in-flight drafts"). Cheap index.
CREATE INDEX IF NOT EXISTS idx_templates_state ON templates(template_state);

-- "Show me templates I created" for the wizard's landing view.
CREATE INDEX IF NOT EXISTS idx_templates_created_by ON templates(created_by);

-- Explicit backfill for clarity. The DEFAULTs above handle this on the live
-- database, but stating the intent in the migration makes the audit trail
-- obvious.
UPDATE templates SET template_state = 'active' WHERE template_state IS NULL OR template_state = '';
UPDATE templates SET source_type = 'manual' WHERE source_type IS NULL OR source_type = '';
