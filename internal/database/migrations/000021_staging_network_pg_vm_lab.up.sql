-- Correct the staging network from the phantom "LabVMs-VLAN30" (a port
-- group name that never existed on any ESXi host) to "PG-VM-Lab", the
-- real VLAN 30 staging port group present on all hosts with DHCP and
-- internet access. Template build VMs attached to the old name came up
-- with a disconnected NIC (unrecoverableError) and never got an IP.
ALTER TABLE templates ALTER COLUMN staging_network SET DEFAULT 'PG-VM-Lab';

UPDATE templates
    SET staging_network = 'PG-VM-Lab'
    WHERE staging_network = 'LabVMs-VLAN30';
