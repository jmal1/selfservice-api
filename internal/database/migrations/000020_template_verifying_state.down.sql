-- Revert the `verifying` lifecycle state. Any template caught mid-verify
-- is moved back to `ready` (a safe, retryable state) so the narrowed CHECK
-- constraint does not reject it.
UPDATE templates SET template_state = 'ready' WHERE template_state = 'verifying';

ALTER TABLE templates
    DROP CONSTRAINT IF EXISTS templates_state_check;
ALTER TABLE templates
    ADD CONSTRAINT templates_state_check
    CHECK (template_state IN ('draft', 'provisioning', 'configuring',
                              'generalizing', 'ready', 'active', 'error'));
