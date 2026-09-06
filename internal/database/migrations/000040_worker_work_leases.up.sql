-- Named work leases for multi-worker exclusivity without process-wide leader election.
-- claimed_at is heartbeated; any replica may reclaim after WorkLeaseDuration.
CREATE TABLE IF NOT EXISTS worker_work_leases (
    name         TEXT PRIMARY KEY,
    claimed_by   TEXT,
    claim_token  UUID,
    claimed_at   TIMESTAMPTZ,
    generation   BIGINT NOT NULL DEFAULT 0
);

INSERT INTO worker_work_leases (name) VALUES
    ('retry_failed_destroys'),
    ('expire_stale'),
    ('stuck_uploads'),
    ('vcenter_orphan'),
    ('template_orphan'),
    ('podvm_ip'),
    ('network_reconcile'),
    ('idle_eval'),
    ('template_metrics'),
    ('retry_pending'),
    ('l1_validation'),
    ('template_health'),
    ('template_health_confirm')
ON CONFLICT (name) DO NOTHING;
