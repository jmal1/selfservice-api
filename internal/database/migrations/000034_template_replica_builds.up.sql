BEGIN;

CREATE TABLE template_source_replica_builds (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id UUID NOT NULL REFERENCES templates(id) ON DELETE CASCADE,
    source_replica_id UUID NOT NULL REFERENCES template_source_replicas(id) ON DELETE RESTRICT,
    result_replica_id UUID REFERENCES template_source_replicas(id) ON DELETE RESTRICT,
    job_id UUID UNIQUE REFERENCES jobs(id) ON DELETE RESTRICT,
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    operation_id UUID NOT NULL UNIQUE,
    canary_operation_id UUID NOT NULL UNIQUE,
    source_vm_moref TEXT NOT NULL CHECK (source_vm_moref ~ '^vm-[0-9]+$'),
    source_snapshot_name TEXT NOT NULL CHECK (source_snapshot_name <> ''),
    source_snapshot_moref TEXT NOT NULL DEFAULT '',
    destination_name TEXT NOT NULL CHECK (destination_name <> ''),
    compute_resource_type TEXT NOT NULL
        CHECK (compute_resource_type IN ('ClusterComputeResource', 'ComputeResource')),
    compute_resource_moref TEXT NOT NULL CHECK (compute_resource_moref <> ''),
    compute_resource_path TEXT NOT NULL CHECK (compute_resource_path <> ''),
    host_moref TEXT NOT NULL CHECK (host_moref <> ''),
    host_name TEXT NOT NULL CHECK (host_name <> ''),
    resource_pool_moref TEXT NOT NULL CHECK (resource_pool_moref <> ''),
    resource_pool_path TEXT NOT NULL CHECK (resource_pool_path <> ''),
    datastore_moref TEXT NOT NULL CHECK (datastore_moref <> ''),
    datastore_name TEXT NOT NULL CHECK (datastore_name <> ''),
    folder_moref TEXT NOT NULL CHECK (folder_moref <> ''),
    folder_path TEXT NOT NULL CHECK (folder_path <> ''),
    provision_datastore TEXT NOT NULL CHECK (provision_datastore <> ''),
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'ready', 'failed', 'cleanup_required')),
    phase TEXT NOT NULL DEFAULT 'pending'
        CHECK (phase IN (
            'pending', 'clone_submitting', 'clone_submitted', 'validating',
            'snapshot_submitting', 'snapshot_submitted', 'canary_prepared',
            'canary_submitting', 'canary_submitted', 'cleanup_prepared',
            'cleanup_submitting', 'cleanup_submitted',
            'residue_prepared', 'residue_submitting', 'residue_submitted', 'residue_cleaned',
            'finalizing', 'ready', 'failed', 'cleanup_required'
        )),
    resume_phase TEXT NOT NULL DEFAULT 'pending',
    clone_task_ref TEXT NOT NULL DEFAULT '',
    destination_vm_moref TEXT NOT NULL DEFAULT '',
    snapshot_task_ref TEXT NOT NULL DEFAULT '',
    destination_snapshot_moref TEXT NOT NULL DEFAULT '',
    canary_task_ref TEXT NOT NULL DEFAULT '',
    canary_vm_moref TEXT NOT NULL DEFAULT '',
    cleanup_task_ref TEXT NOT NULL DEFAULT '',
    cleanup_completed_at TIMESTAMPTZ,
    residue_cleanup_task_ref TEXT NOT NULL DEFAULT '',
    residue_cleaned_at TIMESTAMPTZ,
    last_error_code TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ,
    submission_started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (template_id, idempotency_key),
    CHECK (status <> 'ready' OR (
        phase = 'ready'
        AND result_replica_id IS NOT NULL
        AND cleanup_completed_at IS NOT NULL
        AND destination_snapshot_moref <> ''
        AND canary_vm_moref <> ''
    ))
);

CREATE INDEX idx_template_replica_builds_status
    ON template_source_replica_builds (status, updated_at);

CREATE UNIQUE INDEX idx_template_replica_builds_active_compute
    ON template_source_replica_builds (
        template_id, compute_resource_type, compute_resource_moref
    )
    WHERE status IN ('pending', 'running', 'cleanup_required');

CREATE TRIGGER template_source_replica_builds_touch_updated_at
    BEFORE UPDATE ON template_source_replica_builds
    FOR EACH ROW
    EXECUTE FUNCTION touch_updated_at();

COMMIT;
