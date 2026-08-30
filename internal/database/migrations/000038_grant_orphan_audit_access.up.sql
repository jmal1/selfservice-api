BEGIN;

GRANT INSERT, SELECT ON TABLE pod_destroy_recovery_audit TO selfservice;
GRANT USAGE ON SEQUENCE pod_destroy_recovery_audit_id_seq TO selfservice;

COMMIT;
