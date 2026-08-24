BEGIN;

DROP TRIGGER IF EXISTS template_source_replica_builds_touch_updated_at
    ON template_source_replica_builds;
DROP TABLE IF EXISTS template_source_replica_builds;

COMMIT;
