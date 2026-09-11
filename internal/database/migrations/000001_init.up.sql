-- Self-Service Portal schema
-- Migration 001: Initial tables

BEGIN;

-- Users (synced from OIDC claims on login)
CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    oidc_sub TEXT UNIQUE NOT NULL,
    username TEXT UNIQUE NOT NULL,
    email TEXT NOT NULL,
    display_name TEXT,
    role TEXT NOT NULL DEFAULT 'student',
    max_vcpus INT NOT NULL DEFAULT 4,
    max_ram_mb INT NOT NULL DEFAULT 8192,
    max_pods INT NOT NULL DEFAULT 2,
    is_active BOOLEAN DEFAULT true,
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now()
);

-- Templates (admin-managed, maps to vCenter templates)
CREATE TABLE templates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    vcenter_template TEXT NOT NULL,
    os_type TEXT NOT NULL,
    default_vcpus INT NOT NULL DEFAULT 2,
    default_ram_mb INT NOT NULL DEFAULT 2048,
    default_disk_gb INT NOT NULL DEFAULT 20,
    min_vcpus INT NOT NULL DEFAULT 1,
    min_ram_mb INT NOT NULL DEFAULT 1024,
    description TEXT,
    icon_url TEXT,
    is_active BOOLEAN DEFAULT true,
    created_at TIMESTAMPTZ DEFAULT now()
);

-- Template access (which users/roles can use which templates)
CREATE TABLE template_access (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id UUID REFERENCES templates(id) ON DELETE CASCADE,
    user_id UUID REFERENCES users(id) ON DELETE CASCADE,
    role TEXT,
    CHECK (user_id IS NOT NULL OR role IS NOT NULL)
);

-- Pods (a student's isolated environment: 1 VLAN + N VMs)
CREATE TABLE pods (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id UUID NOT NULL REFERENCES users(id),
    name TEXT NOT NULL,
    pod_index INT UNIQUE NOT NULL,
    vlan_id INT GENERATED ALWAYS AS (100 + pod_index) STORED,
    subnet TEXT GENERATED ALWAYS AS ('10.100.' || pod_index || '.0/24') STORED,
    status TEXT NOT NULL DEFAULT 'pending',
    error_message TEXT,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now()
);

-- VMs within a pod
CREATE TABLE pod_vms (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pod_id UUID NOT NULL REFERENCES pods(id) ON DELETE CASCADE,
    template_id UUID NOT NULL REFERENCES templates(id),
    vcenter_vm_name TEXT,
    vcenter_vm_id TEXT,
    vcpus INT NOT NULL,
    ram_mb INT NOT NULL,
    disk_gb INT NOT NULL,
    ip_address TEXT,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at TIMESTAMPTZ DEFAULT now()
);

-- Jobs (durable task queue)
CREATE TABLE jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type TEXT NOT NULL,
    payload JSONB NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    claimed_by TEXT,
    claimed_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    result JSONB,
    retry_count INT DEFAULT 0,
    max_retries INT DEFAULT 3,
    rollback_steps JSONB DEFAULT '[]',
    created_at TIMESTAMPTZ DEFAULT now()
);

-- Audit log
CREATE TABLE audit_log (
    id BIGSERIAL PRIMARY KEY,
    user_id UUID REFERENCES users(id),
    action TEXT NOT NULL,
    resource_type TEXT,
    resource_id UUID,
    details JSONB,
    ip_address INET,
    created_at TIMESTAMPTZ DEFAULT now()
);

-- Indexes
CREATE INDEX idx_jobs_pending ON jobs(status, created_at) WHERE status = 'pending';
CREATE INDEX idx_pods_owner ON pods(owner_id) WHERE status != 'destroyed';
CREATE INDEX idx_pod_vms_pod ON pod_vms(pod_id);
CREATE INDEX idx_template_access_template ON template_access(template_id);
CREATE INDEX idx_audit_log_user ON audit_log(user_id, created_at);
CREATE INDEX idx_audit_log_resource ON audit_log(resource_type, resource_id);

COMMIT;
