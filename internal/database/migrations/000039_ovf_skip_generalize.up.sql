-- source_type=ovf + skip_generalize for the template wizard.
--
-- ovf is a clone-from-imported-OVA path. source_ref is the vCenter VM moref
-- of an already-imported OVA (same shape as clone_vcenter). Instructors
-- import the OVA via the existing Images / ImportOVA path first.
--
-- skip_generalize, when true, tells template_generalize to power the staging
-- VM off and take the base-image snapshot without running GuestOps
-- generalize scripts. Publish is unchanged: ready → verifying → active.

ALTER TABLE templates
    ADD COLUMN IF NOT EXISTS skip_generalize BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE templates
    DROP CONSTRAINT IF EXISTS templates_source_type_check;
ALTER TABLE templates
    ADD CONSTRAINT templates_source_type_check
    CHECK (source_type IN ('manual', 'clone_template', 'clone_vcenter', 'iso', 'ovf'));
