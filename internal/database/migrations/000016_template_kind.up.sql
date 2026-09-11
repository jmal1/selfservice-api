-- Migration 000016: template kind discriminator
--
-- Adds a `kind` enum to templates so the provisioner can branch between
-- three intake modes:
--   * clone_with_customize    — today's default; clone + sysprep/cloud-init + assigned IP
--   * clone_no_customize      — clone, attach NIC, power on; guest manages its own network
--   * registered_existing_vm  — no clone; the named VM IS the template (linked-clone per pod)
--
-- Adds `assign_ip` so the no-customize / registered-existing flows can opt out of
-- the provisioner's IP assignment without inventing a sentinel value in vcenter_template.
--
-- Backfill is safe: every existing row gets kind = 'clone_with_customize' and
-- assign_ip = true, which is exactly the behavior they have today.

ALTER TABLE templates
  ADD COLUMN kind TEXT NOT NULL DEFAULT 'clone_with_customize'
    CHECK (kind IN ('clone_with_customize', 'clone_no_customize', 'registered_existing_vm'));

ALTER TABLE templates
  ADD COLUMN assign_ip BOOLEAN NOT NULL DEFAULT true;

-- Index `kind` because the admin UI and the provisioner branch on it on every
-- pod-creation request.
CREATE INDEX idx_templates_kind ON templates(kind);

COMMENT ON COLUMN templates.kind IS
  'Provisioning intake mode: clone_with_customize (default sysprep/cloud-init path), clone_no_customize (clone but skip guest customization), or registered_existing_vm (link-clone from a pre-existing VM with static creds).';

COMMENT ON COLUMN templates.assign_ip IS
  'When true, the provisioner assigns an IP from the pod VLAN DHCP scope. When false, the NIC is attached but the guest is responsible for its own networking (DHCP from inside or static config).';
