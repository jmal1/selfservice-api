-- Reverse 000039. Convert any ovf rows to clone_vcenter (same source_ref
-- moref contract) before restoring the narrower CHECK.

UPDATE templates SET source_type = 'clone_vcenter' WHERE source_type = 'ovf';

ALTER TABLE templates
    DROP CONSTRAINT IF EXISTS templates_source_type_check;
ALTER TABLE templates
    ADD CONSTRAINT templates_source_type_check
    CHECK (source_type IN ('manual', 'clone_template', 'clone_vcenter', 'iso'));

ALTER TABLE templates DROP COLUMN IF EXISTS skip_generalize;
