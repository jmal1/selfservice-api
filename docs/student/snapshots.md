# Snapshots and Recovery

Snapshots save a point-in-time state for one VM so you can return to it after an experiment.

## Create a named snapshot

1. Open your lab, choose a VM, and open its snapshots.
2. Select **Create Snapshot**.
3. Enter a short, specific name and an optional description of what you want to preserve.
4. Wait for the snapshot job to finish before making the next major change.

**Expected result:** The named snapshot appears in that VM's snapshot list. Each VM keeps its protected original snapshot plus one snapshot you create. Creating another named snapshot replaces the previous named snapshot only after the new snapshot is created successfully.

## Revert to a named snapshot

1. Save any work you need after the snapshot; reverting discards later changes.
2. Stop the VM and wait until it is powered off.
3. Open snapshots, select the named snapshot, and choose **Revert**.
4. Wait for the revert job, then start the VM.

**Expected result:** The VM returns to the saved state. Files, configuration, and software changes made after that snapshot are lost.

## Restore the initial snapshot

1. Stop the VM and wait until it is powered off.
2. Open snapshots and choose **Restore Initial Snapshot**.
3. Wait for the restore job, then start the VM.

**Expected result:** The VM returns to its original lab state. This removes work made since the lab was created, including work preserved only in later snapshots.

You can delete a named snapshot you no longer need. The initial snapshot is protected and cannot be deleted.
