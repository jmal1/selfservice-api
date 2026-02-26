-- Migration 003: Replace fragile pod_index allocation with VLAN pool
-- VLANs are "checked out" from a pool on pod creation and "released" on destruction.
-- The pool is admin-managed and can be expanded by INSERTing new rows.

BEGIN;

-- Step 1: Create the VLAN pool table (growable, not hard-coded)
CREATE TABLE vlan_pool (
    id SERIAL PRIMARY KEY,
    vlan_tag INT NOT NULL UNIQUE,      -- 802.1Q VLAN tag (e.g. 100-255)
    subnet TEXT NOT NULL UNIQUE,       -- IP subnet (e.g. 10.100.5.0/24)
    host_scope TEXT NOT NULL DEFAULT 'all',  -- 'all' = all hosts, 'switch1' = esxi1/esxi2 only
    pod_id UUID REFERENCES pods(id) ON DELETE SET NULL,
    allocated_at TIMESTAMPTZ
);

-- Pre-populate VLANs 100-120: available on ALL hosts (both switches)
INSERT INTO vlan_pool (vlan_tag, subnet, host_scope)
SELECT v, '10.100.' || (v - 100) || '.0/24', 'all'
FROM generate_series(100, 120) AS v;

-- Pre-populate VLANs 121-255: only available on switch1 hosts (esxi1/esxi2)
INSERT INTO vlan_pool (vlan_tag, subnet, host_scope)
SELECT v, '10.100.' || (v - 100) || '.0/24', 'switch1'
FROM generate_series(121, 255) AS v;

CREATE INDEX idx_vlan_pool_available ON vlan_pool(id) WHERE pod_id IS NULL;
CREATE INDEX idx_vlan_pool_pod ON vlan_pool(pod_id) WHERE pod_id IS NOT NULL;

-- Step 2: Backfill pool allocations from any existing active pods
UPDATE vlan_pool vp SET
    pod_id = p.id,
    allocated_at = p.created_at
FROM pods p
WHERE vp.id = p.pod_index AND p.status NOT IN ('destroyed');

-- Step 3: Replace generated columns with regular columns
-- (PostgreSQL doesn't allow ALTER on generated columns, so drop + re-add)
ALTER TABLE pods DROP COLUMN vlan_id;
ALTER TABLE pods DROP COLUMN subnet;

ALTER TABLE pods ADD COLUMN vlan_id INT NOT NULL DEFAULT 0;
ALTER TABLE pods ADD COLUMN subnet TEXT NOT NULL DEFAULT '';

-- Backfill from pod_index for any existing rows
UPDATE pods SET
    vlan_id = 100 + pod_index,
    subnet = '10.100.' || pod_index || '.0/24';

-- Step 4: Drop pod_index (no longer needed)
ALTER TABLE pods DROP COLUMN pod_index;

-- Remove the defaults now that backfill is done
ALTER TABLE pods ALTER COLUMN vlan_id DROP DEFAULT;
ALTER TABLE pods ALTER COLUMN subnet DROP DEFAULT;

COMMIT;
