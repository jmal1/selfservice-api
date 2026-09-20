-- Reverse 043: restore switch1 scope for 121-255; drop 256-355 if free.

BEGIN;

DELETE FROM vlan_pool
WHERE vlan_tag BETWEEN 256 AND 355
  AND pod_id IS NULL;

UPDATE vlan_pool
SET host_scope = 'switch1'
WHERE vlan_tag BETWEEN 121 AND 255;

COMMIT;
