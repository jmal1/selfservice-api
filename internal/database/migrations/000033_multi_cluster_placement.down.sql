BEGIN;

DROP TABLE IF EXISTS vm_placements;
DROP TRIGGER IF EXISTS template_source_replicas_touch_updated_at ON template_source_replicas;
DROP TRIGGER IF EXISTS template_source_replicas_identity_immutable ON template_source_replicas;
DROP TABLE IF EXISTS template_source_replicas;
DROP TABLE IF EXISTS template_source_replica_policies;
DROP FUNCTION IF EXISTS reject_template_source_replica_identity_update();

COMMIT;
