-- Rollback: restore pod_index + generated columns, drop vlan_pool

BEGIN;

-- Re-add pod_index (backfill from vlan_id)
ALTER TABLE pods ADD COLUMN pod_index INT;
UPDATE pods SET pod_index = vlan_id - 100;
ALTER TABLE pods ALTER COLUMN pod_index SET NOT NULL;
ALTER TABLE pods ADD CONSTRAINT pods_pod_index_key UNIQUE (pod_index);

-- Drop regular vlan_id/subnet columns
ALTER TABLE pods DROP COLUMN vlan_id;
ALTER TABLE pods DROP COLUMN subnet;

-- Re-create generated columns
ALTER TABLE pods ADD COLUMN vlan_id INT GENERATED ALWAYS AS (100 + pod_index) STORED;
ALTER TABLE pods ADD COLUMN subnet TEXT GENERATED ALWAYS AS ('10.100.' || pod_index || '.0/24') STORED;

-- Drop pool table
DROP TABLE IF EXISTS vlan_pool;

COMMIT;
