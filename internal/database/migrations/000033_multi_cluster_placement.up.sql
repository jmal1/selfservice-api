BEGIN;

-- A logical Crucible template may have one independently validated source VM
-- in each compute resource. No policy or replica rows are backfilled: legacy
-- templates continue through the guarded single-source fallback until an
-- operator registers the first real replica.
CREATE TABLE template_source_replica_policies (
    template_id UUID PRIMARY KEY REFERENCES templates(id) ON DELETE CASCADE,
    enabled_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE template_source_replicas (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id UUID NOT NULL REFERENCES templates(id) ON DELETE CASCADE,
    source_vm_moref TEXT NOT NULL CHECK (source_vm_moref ~ '^vm-[0-9]+$'),
    compute_resource_type TEXT NOT NULL
        CHECK (compute_resource_type IN ('ClusterComputeResource', 'ComputeResource')),
    compute_resource_moref TEXT NOT NULL CHECK (compute_resource_moref <> ''),
    compute_resource_path TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'ready', 'unhealthy', 'disabled')),
    last_validated_at TIMESTAMPTZ,
    last_validation_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (template_id, compute_resource_type, compute_resource_moref),
    UNIQUE (template_id, source_vm_moref),
    CHECK (
        status <> 'ready'
        OR (last_validated_at IS NOT NULL AND last_validation_error IS NULL)
    )
);

CREATE INDEX idx_template_source_replicas_ready
    ON template_source_replicas (template_id, compute_resource_moref)
    WHERE status = 'ready';

CREATE OR REPLACE FUNCTION reject_template_source_replica_identity_update()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.template_id IS DISTINCT FROM OLD.template_id
       OR NEW.source_vm_moref IS DISTINCT FROM OLD.source_vm_moref
       OR NEW.compute_resource_type IS DISTINCT FROM OLD.compute_resource_type
       OR NEW.compute_resource_moref IS DISTINCT FROM OLD.compute_resource_moref THEN
        RAISE EXCEPTION 'template source replica identity is immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER template_source_replicas_identity_immutable
    BEFORE UPDATE ON template_source_replicas
    FOR EACH ROW
    EXECUTE FUNCTION reject_template_source_replica_identity_update();

CREATE TRIGGER template_source_replicas_touch_updated_at
    BEFORE UPDATE ON template_source_replicas
    FOR EACH ROW
    EXECUTE FUNCTION touch_updated_at();

-- The complete placement decision is durable before a pod portgroup or clone
-- is created. It remains after clone adoption so later VM operations can
-- detect host or DRS-control drift.
CREATE TABLE vm_placements (
    pod_vm_id UUID PRIMARY KEY REFERENCES pod_vms(id) ON DELETE CASCADE,
    job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE RESTRICT,
    template_id UUID NOT NULL REFERENCES templates(id) ON DELETE RESTRICT,
    source_replica_id UUID REFERENCES template_source_replicas(id) ON DELETE RESTRICT,
    source_ref TEXT NOT NULL CHECK (source_ref <> ''),
    compute_resource_type TEXT NOT NULL
        CHECK (compute_resource_type IN ('ClusterComputeResource', 'ComputeResource')),
    compute_resource_moref TEXT NOT NULL CHECK (compute_resource_moref <> ''),
    resource_pool_moref TEXT NOT NULL CHECK (resource_pool_moref <> ''),
    host_moref TEXT NOT NULL CHECK (host_moref <> ''),
    host_name TEXT NOT NULL CHECK (host_name <> ''),
    drs_control TEXT NOT NULL
        CHECK (drs_control IN ('disabled', 'standalone')),
    observed_free_memory_mb BIGINT NOT NULL CHECK (observed_free_memory_mb >= 0),
    reserved_memory_mb BIGINT NOT NULL CHECK (reserved_memory_mb >= 0),
    capacity_reservation_mb BIGINT NOT NULL CHECK (capacity_reservation_mb >= 0),
    capacity_observed_at TIMESTAMPTZ NOT NULL,
    capacity_released_at TIMESTAMPTZ,
    admitted_headroom_mb BIGINT NOT NULL CHECK (admitted_headroom_mb >= 0),
    legacy_adoption_pending BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (job_id, pod_vm_id),
    CHECK (NOT legacy_adoption_pending OR capacity_reservation_mb = 0)
);

CREATE INDEX idx_vm_placements_job ON vm_placements (job_id);
CREATE INDEX idx_vm_placements_host ON vm_placements (host_moref);

COMMIT;
