-- Migration 000027: L1 trust tier + periodic revalidation state on templates.
--
-- trust_tier categorises a template's automated validation regime:
--   l1        — first-class: periodic smoke-clone revalidation is enabled;
--               a stale or failing result fires an alert (see
--               internal/provisioner/trust_validation_reconcile.go).
--   derived   — inherits its quality signal from its source template rather
--               than running its own validation (reserved for future use).
--   untrusted — no automated validation; human review only. Default for
--               existing and new templates.
--
-- last_validated_at  — wall-clock timestamp of the most recent completed
--                      validation run (pass or fail). NULL means validation
--                      has never run. The staleness alert fires when this is
--                      older than the configured interval or is NULL.
--
-- last_validation_result — short human-readable outcome written by the
--                           template_revalidate job: "pass" or a truncated
--                           "fail: <reason>" string. NULL until the first run.

ALTER TABLE templates
    ADD COLUMN IF NOT EXISTS trust_tier TEXT NOT NULL DEFAULT 'untrusted'
        CHECK (trust_tier IN ('l1', 'derived', 'untrusted')),
    ADD COLUMN IF NOT EXISTS last_validated_at      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_validation_result TEXT;
