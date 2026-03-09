CREATE TABLE pod_attestations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pod_id UUID NOT NULL REFERENCES pods(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id),
    previous_expires_at TIMESTAMPTZ,
    new_expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE INDEX idx_pod_attestations_pod ON pod_attestations(pod_id);
