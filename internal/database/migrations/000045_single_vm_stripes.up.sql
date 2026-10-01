-- Single VM stripes. Tags 347-355 leave the isolated 10.100 pool.
-- Their addresses live only in 10.110.0.0/16. Student create never inserts
-- these rows; the provision job marks them active after the networks exist.

BEGIN;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM vlan_pool
        WHERE vlan_tag BETWEEN 347 AND 355
          AND pod_id IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'vlan tags 347-355 are still allocated to a pod';
    END IF;
END $$;

DELETE FROM vlan_pool WHERE vlan_tag BETWEEN 347 AND 355;

CREATE TABLE shared_address_space (
    id INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    cidr TEXT NOT NULL DEFAULT '10.110.0.0/16',
    policy_ready BOOLEAN NOT NULL DEFAULT false
);

INSERT INTO shared_address_space (id, cidr, policy_ready)
VALUES (1, '10.110.0.0/16', false);

CREATE TABLE shared_networks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    vlan_tag INT NOT NULL UNIQUE CHECK (vlan_tag BETWEEN 347 AND 355),
    cidr TEXT NOT NULL UNIQUE,
    gateway TEXT NOT NULL,
    portgroup_name TEXT NOT NULL UNIQUE,
    dhcp_start TEXT NOT NULL,
    dhcp_end TEXT NOT NULL,
    runner_first TEXT NOT NULL,
    runner_last TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'provisioning', 'active', 'error')),
    error_message TEXT NOT NULL DEFAULT '',
    opnsense_vlan_uuid TEXT NOT NULL DEFAULT '',
    opnsense_interface TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE shared_network_portgroup_receipts (
    shared_network_id UUID NOT NULL REFERENCES shared_networks(id),
    host_moref TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'planned'
        CHECK (state IN ('planned', 'applying', 'active', 'removed')),
    receipt JSONB NOT NULL,
    PRIMARY KEY (shared_network_id, host_moref)
);

ALTER TABLE pods ADD COLUMN network_mode TEXT NOT NULL DEFAULT 'isolated'
    CHECK (network_mode IN ('isolated', 'shared'));
ALTER TABLE pods ADD COLUMN shared_network_id UUID REFERENCES shared_networks(id);
ALTER TABLE pods ADD CONSTRAINT pods_shared_network_mode_check CHECK (
    (network_mode = 'isolated' AND shared_network_id IS NULL)
    OR (network_mode = 'shared' AND shared_network_id IS NOT NULL)
);

DROP INDEX idx_pods_vlan_active;
CREATE UNIQUE INDEX idx_pods_vlan_active ON pods (vlan_id)
    WHERE status NOT IN ('destroyed') AND network_mode = 'isolated';

CREATE TABLE runner_ip_leases (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    shared_network_id UUID NOT NULL REFERENCES shared_networks(id),
    ip TEXT NOT NULL,
    run_id UUID,
    leased_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX runner_ip_leases_active
    ON runner_ip_leases (shared_network_id, ip)
    WHERE released_at IS NULL;

COMMIT;
