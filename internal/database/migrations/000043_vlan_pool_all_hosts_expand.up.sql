-- Migration 043: Pod VLANs are trunked on both switches now.
-- switch01 carries pod tags through 355; switch02 through 500.
-- Drop the historical switch1-only restriction on tags 121-255 and grow the
-- pool through 355 (matches 10.100.(tag-100).0/24 and fwpod student range).

BEGIN;

UPDATE vlan_pool
SET host_scope = 'all'
WHERE host_scope = 'switch1';

INSERT INTO vlan_pool (vlan_tag, subnet, host_scope)
SELECT v, '10.100.' || (v - 100) || '.0/24', 'all'
FROM generate_series(256, 355) AS v
ON CONFLICT (vlan_tag) DO UPDATE
SET host_scope = 'all',
    subnet = EXCLUDED.subnet;

COMMIT;
