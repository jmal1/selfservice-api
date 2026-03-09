-- Blueprint definitions (reusable pod templates)
CREATE TABLE blueprints (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_by UUID NOT NULL REFERENCES users(id),
    allow_vm_additions BOOLEAN NOT NULL DEFAULT false,
    is_active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now()
);

-- VMs within a blueprint
CREATE TABLE blueprint_vms (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    blueprint_id UUID NOT NULL REFERENCES blueprints(id) ON DELETE CASCADE,
    template_id UUID NOT NULL REFERENCES templates(id),
    display_name TEXT NOT NULL,
    vcpus INT,
    ram_mb INT,
    disk_gb INT,
    boot_order INT NOT NULL DEFAULT 0,
    quantity INT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE INDEX idx_blueprint_vms_blueprint ON blueprint_vms(blueprint_id);

-- Blueprint access control (same pattern as template_access)
CREATE TABLE blueprint_access (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    blueprint_id UUID NOT NULL REFERENCES blueprints(id) ON DELETE CASCADE,
    user_id UUID REFERENCES users(id) ON DELETE CASCADE,
    role TEXT,
    CHECK (user_id IS NOT NULL OR role IS NOT NULL)
);

-- Pod tracks its source blueprint and VM addition policy
ALTER TABLE pods ADD COLUMN blueprint_id UUID REFERENCES blueprints(id);
ALTER TABLE pods ADD COLUMN allow_vm_additions BOOLEAN NOT NULL DEFAULT true;
