BEGIN;

REVOKE INSERT, SELECT ON TABLE pod_destroy_recovery_audit FROM selfservice;
REVOKE USAGE ON SEQUENCE pod_destroy_recovery_audit_id_seq FROM selfservice;

COMMIT;
