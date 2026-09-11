-- VM Snapshots table for user-initiated and initial provisioning snapshots
CREATE TABLE vm_snapshots (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pod_vm_id UUID NOT NULL REFERENCES pod_vms(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    vcenter_snapshot_id TEXT NOT NULL,
    is_initial BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_vm_snapshots_pod_vm_id ON vm_snapshots(pod_vm_id);

-- Only one initial snapshot per VM
CREATE UNIQUE INDEX idx_vm_snapshots_initial ON vm_snapshots(pod_vm_id) WHERE is_initial = true;
