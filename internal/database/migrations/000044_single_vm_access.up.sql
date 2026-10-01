-- Single VM access flags. Defaults keep today's behavior: every student
-- still has labs until Helm admission.labsRequireGrant is true, and every
-- template can still be used in a blueprint until single_vm_only is set.

BEGIN;

ALTER TABLE users
    ADD COLUMN labs_enabled BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN max_single_vms INT NOT NULL DEFAULT 1;

ALTER TABLE users
    ADD CONSTRAINT users_max_single_vms_range CHECK (max_single_vms BETWEEN 0 AND 3);

ALTER TABLE templates
    ADD COLUMN single_vm_only BOOLEAN NOT NULL DEFAULT false;

COMMIT;
