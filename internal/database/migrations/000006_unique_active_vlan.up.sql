-- Prevent two non-destroyed pods from sharing the same VLAN.
-- Destroyed pods can share VLANs (they've released theirs back to the pool).
CREATE UNIQUE INDEX idx_pods_vlan_active ON pods(vlan_id)
    WHERE status NOT IN ('destroyed');
