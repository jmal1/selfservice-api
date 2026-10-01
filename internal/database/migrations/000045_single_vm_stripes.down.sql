BEGIN;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pods WHERE network_mode = 'shared' AND status <> 'destroyed'
    ) THEN
        RAISE EXCEPTION 'shared pods still exist';
    END IF;
END $$;

DROP TABLE IF EXISTS runner_ip_leases;
DROP INDEX IF EXISTS idx_pods_vlan_active;
ALTER TABLE pods DROP CONSTRAINT IF EXISTS pods_shared_network_mode_check;
ALTER TABLE pods DROP COLUMN IF EXISTS shared_network_id;
ALTER TABLE pods DROP COLUMN IF EXISTS network_mode;
DROP TABLE IF EXISTS shared_network_portgroup_receipts;
DROP TABLE IF EXISTS shared_networks;
DROP TABLE IF EXISTS shared_address_space;

CREATE UNIQUE INDEX idx_pods_vlan_active ON pods (vlan_id)
    WHERE status NOT IN ('destroyed');

INSERT INTO vlan_pool (vlan_tag, subnet, host_scope)
SELECT v, '10.100.' || (v - 100) || '.0/24', 'all'
FROM generate_series(347, 355) AS v
ON CONFLICT (vlan_tag) DO NOTHING;

COMMIT;
