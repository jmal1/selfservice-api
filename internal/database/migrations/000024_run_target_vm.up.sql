-- Record which pod VM an assessment run was executed against.
--
-- Before this, nothing anywhere identified the target. runs.runner_vm_id /
-- runner_vm_name describe the *Kali runner Job* ("crucible-runner-<run>"), not
-- the machine being graded, and workflow_results had no target column at all.
-- The engine's own GetRunTargetInfo query selected only ip/os/username/password,
-- so it never learned which pod_vms row it had picked either.
--
-- The practical consequence: for a multi-VM pod there was no way -- in the UI,
-- the API, or the database -- to tell which VM a student was graded on. The only
-- trace was incidental, e.g. nmap happening to print the scanned IP into its
-- stdout inside action_results.
--
-- target_vm_name and target_vm_ip are deliberately denormalized copies rather
-- than being read back through the FK. Destroying a pod removes its pod_vms
-- rows, which would otherwise erase the attribution of every historical run for
-- that pod -- exactly the records an instructor needs after the lab is over.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS target_pod_vm_id UUID
    REFERENCES pod_vms(id) ON DELETE SET NULL;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS target_vm_name TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS target_vm_ip TEXT NOT NULL DEFAULT '';
