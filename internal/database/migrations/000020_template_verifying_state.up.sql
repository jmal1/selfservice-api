-- Template smoke-test gate (T4 wizard ergonomics) — adds the `verifying`
-- lifecycle state.
--
-- Publishing a template now goes through an automated smoke test that
-- clones the freshly-generalized base-image, boots it, and waits for
-- VMware Tools + an IP before flipping the template live. This catches
-- "bricked image" regressions (sysprep left the OS unbootable, no network,
-- BitLocker still on, etc.) BEFORE any student ever clones it, instead of
-- hours later when the first pod fails.
--
-- State flow change:
--   old:  ready → active            (publish flipped is_active directly)
--   new:  ready → verifying → active (verify worker gates the publish)
--
-- Backwards compatible: no existing row is in `verifying`, and the only
-- change is widening the CHECK constraint enum.
ALTER TABLE templates
    DROP CONSTRAINT IF EXISTS templates_state_check;
ALTER TABLE templates
    ADD CONSTRAINT templates_state_check
    CHECK (template_state IN ('draft', 'provisioning', 'configuring',
                              'generalizing', 'ready', 'verifying',
                              'active', 'error'));
