ALTER TABLE templates ALTER COLUMN staging_network SET DEFAULT 'LabVMs-VLAN30';

UPDATE templates
    SET staging_network = 'LabVMs-VLAN30'
    WHERE staging_network = 'PG-VM-Lab';
