-- Smoke playlist for the `runner_smoke` synthetic check (plan Epic D / T1).
--
-- WHAT THIS EXISTS FOR
--
-- `runner_smoke` is the only check that exercises the whole Kali-runner path
-- end to end: resolve template -> create pod -> wait active -> trigger this
-- playlist -> poll to a terminal run state -> assert results exist -> destroy.
-- That path spans Multus, the macvlan NAD, CNI DHCP on the pod VLAN, the GHCR
-- image pull on k3sv03, the runner->engine callback, and the action library
-- generation in /opt/crucible/lib/actions.sh. Nothing monitored it before.
--
-- WHY FIXED UUIDs
--
-- The playlist id is passed to the monitor CronJob as
-- SYNTHETIC_RUNNER_PLAYLIST_ID in the Helm values. A generated id would have to
-- be looked up and copied by hand into values.yaml after every environment
-- rebuild, which is exactly the kind of manual step that drifts. Pinning it
-- here makes the Helm value a constant that can be reviewed in a diff.
--
-- WHY THIS IS CONVERGENT (DO UPDATE), NOT MERELY IDEMPOTENT (DO NOTHING)
--
-- The script below IS the check's assertion set. If someone edits the workflow
-- in the database, the check silently starts asserting something other than
-- what is in version control -- and, because it would still be green, nobody
-- would notice. Re-applying this file therefore RESTORES the committed
-- definition rather than skipping. The trade-off is that "re-run reports
-- INSERT 0 0" is not the idempotency proof here; the proof is that the row
-- count stays at 1 and the stored script matches this file.
--
-- WHY THESE FOUR ACTIONS, IN THIS ORDER
--
-- Ordered strictly innermost -> outermost, so the FIRST failing action names
-- the layer that broke and the Discord alert is actionable without a kubectl
-- session:
--
--   1. Runner image      -- Kali base + internal/runnertools/tools.txt.
--                           Fails if the image build regressed or a tool was
--                           dropped from the manifest.
--   2. Pod-VLAN NIC      -- Multus attached the NAD AND the CNI dhcp plugin got
--                           a lease. This is the single most fragile piece of
--                           Epic D and has no other continuous coverage.
--   3. L3 to the target  -- routing from the runner's net1 to the pod's VM.
--                           This is what silently broke when every pod VLAN was
--                           landing on the same OPNsense interface (W4-OUT-1).
--   4. nmap actually scans -- proves nmap is not merely present but USABLE.
--                           The image strips file capabilities, so an unusable
--                           nmap would still satisfy check 1 while every
--                           student assessment using nmap-service failed.
--
-- Checks 1 and 4 deliberately overlap on nmap: "installed" and "can scan" are
-- different failures with different fixes, and collapsing them would make the
-- capability regression indistinguishable from a missing package.
--
-- WHY IT IS SAFE THAT THIS RUNS AGAINST A REAL POD
--
-- The pod is created and destroyed by the check itself from the
-- `synthetic-noop` template, using the mandatory `synthetic-noop-` name prefix
-- so the synthetic_janitor CronJob sweeps any leak. `visible_to_students` is
-- false and the workflow is linked ONLY to this playlist, so it can never
-- appear in a student's assessment.
--
-- This is NOT a schema migration; it inserts data rows. Apply with:
--
--   PW=$(kubectl -n selfservice get secret selfservice-db-creds \
--          -o jsonpath='{.data.password}' | base64 -d)
--   kubectl -n selfservice cp deploy/sql/synthetic-runner-smoke-playlist.sql \
--     selfservice-postgresql-0:/tmp/runner-smoke.sql
--   kubectl -n selfservice exec selfservice-postgresql-0 -- \
--     env PGPASSWORD="$PW" psql -U selfservice -d selfservice \
--     -f /tmp/runner-smoke.sql
--
-- Then set in deploy/helm/selfservice/values.yaml:
--   synthetic.runner.playlistId: 5e7c0a00-0000-4000-a000-000000000001
--   synthetic.runner.templateName: synthetic-noop

BEGIN;

-- The owning identity is the read-only instructor synthetic user (quotas all
-- zero, no OIDC sub). Created by synthetic-instructor-user.sql; fail loudly
-- rather than silently seeding an unowned playlist.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM users WHERE username = 'synthetic-instructor') THEN
        RAISE EXCEPTION
            'synthetic-instructor user is missing; apply deploy/sql/synthetic-instructor-user.sql first';
    END IF;
END $$;

INSERT INTO workflows (
    id,
    name,
    slug,
    description,
    category,
    execution_mode,
    script,
    timeout_seconds,
    visible_to_students,
    status,
    creation_mode,
    created_by,
    is_active
) VALUES (
    '5e7c0a00-0000-4000-a000-000000000002',
    'Runner Smoke',
    'runner-smoke',
    'Synthetic monitoring only. Proves the Kali runner path end to end: image, pod-VLAN NIC, L3 to the target, and that nmap can actually scan.',
    'general',
    'kali_runner',
    -- NOTE: explicit `||` between the lines. PostgreSQL implicitly concatenates
    -- adjacent plain '...' literals separated by a newline, but NOT E'...'
    -- ones -- the E prefix starts a new literal, so the implicit form is a
    -- syntax error. The escapes stay E-quoted so the stored script always uses
    -- real \n regardless of this file's line endings; a stray CR here would
    -- otherwise produce `#!/bin/bash\r` and a "bad interpreter" failure that
    -- looks nothing like an encoding problem.
    E'#!/bin/bash\n' ||
    E'source /opt/crucible/lib/actions.sh\n' ||
    E'\n' ||
    E'# 1. The runner container itself: Kali base image + the tools manifest.\n' ||
    E'run_action "Runner Image Has Kali Tooling" command_check --cmd "nmap --version" --expect-output "Nmap version"\n' ||
    E'\n' ||
    E'# 2. Multus attached the macvlan NAD AND the CNI dhcp plugin got a lease.\n' ||
    E'run_action "Runner Has Pod-VLAN Address" command_check --cmd "ip -4 -o addr show net1" --expect-output "inet "\n' ||
    E'\n' ||
    E'# 3. L3 from the runner net1 to the pod VM on the pod VLAN.\n' ||
    E'run_action "Target Reachable On Pod VLAN" port_open --host "$CRUCIBLE_TARGET_IP" --port 22\n' ||
    E'\n' ||
    E'# 4. nmap is not merely installed but usable after the capability strip.\n' ||
    E'run_action "Nmap Can Scan The Target" nmap_service --host "$CRUCIBLE_TARGET_IP" --port 22 --expect-service ssh\n',
    180,
    false,
    'active',
    'visual',
    (SELECT id FROM users WHERE username = 'synthetic-instructor'),
    true
) ON CONFLICT (id) DO UPDATE SET
    name                = EXCLUDED.name,
    description         = EXCLUDED.description,
    script              = EXCLUDED.script,
    timeout_seconds     = EXCLUDED.timeout_seconds,
    visible_to_students = EXCLUDED.visible_to_students,
    status              = EXCLUDED.status,
    is_active           = EXCLUDED.is_active,
    updated_at          = now();

INSERT INTO playlists (
    id,
    name,
    slug,
    description,
    scoring_mode,
    created_by,
    is_active
) VALUES (
    '5e7c0a00-0000-4000-a000-000000000001',
    'Runner Smoke (synthetic)',
    'runner-smoke-synthetic',
    'Synthetic monitoring only. Triggered hourly by the runner_smoke check; not for student use.',
    'pass_fail',
    (SELECT id FROM users WHERE username = 'synthetic-instructor'),
    true
) ON CONFLICT (id) DO UPDATE SET
    name        = EXCLUDED.name,
    description = EXCLUDED.description,
    is_active   = EXCLUDED.is_active,
    updated_at  = now();

INSERT INTO playlist_workflows (playlist_id, workflow_id, execution_order)
VALUES (
    '5e7c0a00-0000-4000-a000-000000000001',
    '5e7c0a00-0000-4000-a000-000000000002',
    1
) ON CONFLICT (playlist_id, workflow_id) DO UPDATE SET
    execution_order = EXCLUDED.execution_order;

-- Visibility only. CreateTestingRun does NOT validate the playlist<->template
-- join, so the check works without this row; it exists so a human debugging a
-- red runner_smoke can find the playlist on the synthetic-noop template in the
-- UI instead of going to the database.
INSERT INTO template_playlists (template_id, playlist_id, execution_order)
SELECT t.id, '5e7c0a00-0000-4000-a000-000000000001', 1
FROM templates t
WHERE t.name = 'synthetic-noop'
ON CONFLICT (template_id, playlist_id) DO NOTHING;

COMMIT;
