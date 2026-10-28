BEGIN;

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

COMMIT;
