BEGIN;

CREATE TABLE pod_portgroup_receipts (
    pod_id UUID PRIMARY KEY REFERENCES pods(id) ON DELETE CASCADE,
    create_job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE RESTRICT,
    receipt JSONB NOT NULL,
    portgroup_keys JSONB NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(portgroup_keys) = 'object'),
    state TEXT NOT NULL DEFAULT 'planned'
        CHECK (state IN ('legacy', 'planned', 'applying', 'active', 'removed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    removed_at TIMESTAMPTZ,
    CHECK (
        jsonb_typeof(receipt) = 'object'
        AND receipt ? 'name'
        AND receipt ? 'vlan_id'
        AND jsonb_typeof(receipt->'hosts') = 'array'
        AND jsonb_array_length(receipt->'hosts') > 0
    ),
    CHECK (
        (state IN ('legacy', 'planned', 'applying', 'active') AND removed_at IS NULL)
        OR (state = 'removed' AND removed_at IS NOT NULL)
    )
);

-- Preserve any still-pending ownership receipts from jobs created before this
-- migration. Completed rollback steps may already have been checkpointed out;
-- those require operator-supplied exact evidence and are intentionally not
-- inferred from current inventory or a portgroup name.
INSERT INTO pod_portgroup_receipts (pod_id, create_job_id, receipt, state)
SELECT DISTINCT ON ((j.payload->>'pod_id')::uuid)
       (j.payload->>'pod_id')::uuid,
       j.id,
       jsonb_set(
           step.value->'data',
           '{hosts}',
           normalized.hosts
       ),
       'legacy'
FROM jobs j
CROSS JOIN LATERAL jsonb_array_elements(
    COALESCE(j.rollback_steps, '[]'::jsonb)
) WITH ORDINALITY AS step(value, position)
CROSS JOIN LATERAL (
    SELECT jsonb_agg(
        host.value
        || jsonb_build_object(
            'vswitch_name', COALESCE(host.value->>'vswitch_name', 'vSwitch0'),
            'security', COALESCE(
                host.value->'security',
                jsonb_build_object(
                    'allow_promiscuous', NULL,
                    'mac_changes', NULL,
                    'forged_transmits', NULL
                )
            )
        )
        ORDER BY host.position
    ) AS hosts
    FROM jsonb_array_elements(step.value->'data'->'hosts')
         WITH ORDINALITY AS host(value, position)
) AS normalized
WHERE j.type = 'pod_create'
  AND j.payload->>'pod_id' IS NOT NULL
  AND j.payload->>'pod_id' ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
  AND step.value->>'name' = 'portgroup_create'
ORDER BY (j.payload->>'pod_id')::uuid, j.created_at ASC, step.position ASC
ON CONFLICT (pod_id) DO NOTHING;

CREATE OR REPLACE FUNCTION reject_pod_portgroup_receipt_identity_update()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.pod_id IS DISTINCT FROM OLD.pod_id
       OR NEW.create_job_id IS DISTINCT FROM OLD.create_job_id
       OR NEW.receipt IS DISTINCT FROM OLD.receipt
       OR NOT (NEW.portgroup_keys @> OLD.portgroup_keys)
       OR (OLD.state = 'removed' AND NEW.state <> 'removed')
       OR (
           NEW.state IS DISTINCT FROM OLD.state
           AND NOT (
               (OLD.state = 'planned' AND NEW.state IN ('applying', 'removed'))
               OR (OLD.state = 'applying' AND NEW.state = 'active')
               OR (OLD.state = 'active' AND NEW.state = 'removed')
           )
       )
       OR (
           OLD.removed_at IS NOT NULL
           AND NEW.removed_at IS DISTINCT FROM OLD.removed_at
       ) THEN
        RAISE EXCEPTION 'pod portgroup receipt identity and tombstone are immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER pod_portgroup_receipts_identity_immutable
    BEFORE UPDATE ON pod_portgroup_receipts
    FOR EACH ROW
    EXECUTE FUNCTION reject_pod_portgroup_receipt_identity_update();

COMMIT;
