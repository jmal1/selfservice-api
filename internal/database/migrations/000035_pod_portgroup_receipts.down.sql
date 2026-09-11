BEGIN;

DROP TRIGGER IF EXISTS pod_portgroup_receipts_identity_immutable
    ON pod_portgroup_receipts;
DROP FUNCTION IF EXISTS reject_pod_portgroup_receipt_identity_update();
DROP TABLE IF EXISTS pod_portgroup_receipts;

COMMIT;
