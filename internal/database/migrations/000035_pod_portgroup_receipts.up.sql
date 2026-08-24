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
-- inferred from current inventory or a portgroup name. Malformed pre-ledger
-- steps are left unowned so the existing manual-cleanup path remains the only
-- safe recovery option.
INSERT INTO pod_portgroup_receipts (pod_id, create_job_id, receipt, state)
SELECT DISTINCT ON ((j.payload->>'pod_id')::uuid)
       (j.payload->>'pod_id')::uuid,
       j.id,
       step.value->'data',
       'legacy'
FROM jobs j
CROSS JOIN LATERAL jsonb_array_elements(
    CASE
        WHEN jsonb_typeof(j.rollback_steps) = 'array' THEN j.rollback_steps
        ELSE '[]'::jsonb
    END
) WITH ORDINALITY AS step(value, position)
WHERE j.type = 'pod_create'
  AND j.payload->>'pod_id' IS NOT NULL
  AND j.payload->>'pod_id' ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
  AND jsonb_typeof(step.value) = 'object'
  AND step.value->>'name' = 'portgroup_create'
  AND jsonb_typeof(step.value->'data') = 'object'
  AND jsonb_typeof(step.value->'data'->'name') = 'string'
  AND btrim(step.value->'data'->>'name') <> ''
  AND jsonb_typeof(step.value->'data'->'vlan_id') = 'number'
  AND CASE
          WHEN step.value->'data'->>'vlan_id' ~ '^[0-9]{1,4}$'
              THEN (step.value->'data'->>'vlan_id')::integer BETWEEN 1 AND 4094
          ELSE false
      END
  AND CASE
          WHEN jsonb_typeof(step.value->'data'->'hosts') = 'array'
              THEN jsonb_array_length(step.value->'data'->'hosts') > 0
          ELSE false
      END
  AND NOT EXISTS (
      SELECT 1
      FROM jsonb_array_elements(
          CASE
              WHEN jsonb_typeof(step.value->'data'->'hosts') = 'array'
                  THEN step.value->'data'->'hosts'
              ELSE '[]'::jsonb
          END
      ) AS host(value)
      WHERE jsonb_typeof(host.value) IS DISTINCT FROM 'object'
         OR jsonb_typeof(host.value->'host_name') IS DISTINCT FROM 'string'
         OR btrim(host.value->>'host_name') = ''
         OR jsonb_typeof(host.value->'host_moref') IS DISTINCT FROM 'string'
         OR btrim(host.value->>'host_moref') = ''
         OR jsonb_typeof(host.value->'compute_moref') IS DISTINCT FROM 'string'
         OR btrim(host.value->>'compute_moref') = ''
         OR jsonb_typeof(host.value->'vswitch_name') IS DISTINCT FROM 'string'
         OR btrim(host.value->>'vswitch_name') = ''
         OR jsonb_typeof(host.value->'security') IS DISTINCT FROM 'object'
         OR NOT (host.value->'security' ? 'allow_promiscuous')
         OR NOT (host.value->'security' ? 'mac_changes')
         OR NOT (host.value->'security' ? 'forged_transmits')
         OR COALESCE(
                jsonb_typeof(host.value->'security'->'allow_promiscuous')
                    IN ('boolean', 'null'),
                false
            ) = false
         OR COALESCE(
                jsonb_typeof(host.value->'security'->'mac_changes')
                    IN ('boolean', 'null'),
                false
            ) = false
         OR COALESCE(
                jsonb_typeof(host.value->'security'->'forged_transmits')
                    IN ('boolean', 'null'),
                false
            ) = false
         OR jsonb_typeof(host.value->'preexisting') IS DISTINCT FROM 'boolean'
         OR (
                host.value ? 'portgroup_key'
                AND jsonb_typeof(host.value->'portgroup_key') IS DISTINCT FROM 'string'
            )
  )
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
