-- Keep the primary synthetic monitor above the synthetic-noop template floor.
-- synthetic-noop requires at least 1 vCPU / 1024 MiB. The production recovery
-- uses 3 vCPU / 3072 MiB / 3 pods so lifecycle create/cleanup has headroom.
-- GREATEST preserves any larger operator-assigned quota.
UPDATE users
SET max_vcpus = GREATEST(max_vcpus, 3),
    max_ram_mb = GREATEST(max_ram_mb, 3072),
    max_pods = GREATEST(max_pods, 3),
    updated_at = now()
WHERE oidc_sub = 'synthetic-monitor-no-oidc'
   OR username = 'synthetic';
