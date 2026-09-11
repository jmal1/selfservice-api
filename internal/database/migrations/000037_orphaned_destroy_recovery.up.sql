BEGIN;

CREATE TABLE pod_destroy_recovery_audit (
    id BIGSERIAL PRIMARY KEY,
    pod_id UUID NOT NULL REFERENCES pods(id) ON DELETE CASCADE,
    job_id UUID REFERENCES jobs(id) ON DELETE SET NULL,
    actor_user_id UUID NOT NULL REFERENCES users(id),
    attestation JSONB NOT NULL,
    before_state JSONB NOT NULL,
    after_state JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_pod_destroy_recovery_audit_pod ON pod_destroy_recovery_audit(pod_id, created_at DESC);

COMMIT;
