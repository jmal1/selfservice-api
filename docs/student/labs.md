# Creating and Managing Labs

Create a lab from the templates or blueprints your instructor has shared with you.

## Create a lab

1. On the Dashboard, select a template for one VM or a blueprint for a prepared multi-VM lab.
2. Review the visible name and description, enter a lab name, and select **Deploy**.
3. Watch the deployment progress, then open the lab from **My Labs** when it becomes active.

**Expected result:** Your lab appears in **My Labs** with one or more VM cards.

## Manage a VM

1. Open your lab and select the VM you want to manage.
2. Use the available power action: **Start**, **Stop**, **Restart**, or **Reset**.
3. Wait for the VM status to update before connecting again. A reset is a hard restart and can lose unsaved work.

**Expected result:** The selected VM reaches the requested power state.

## Watch expiration and remove a lab

1. Check the expiration time shown on your lab.
2. If your active lab needs more time, select **Extend** and confirm the request. Crucible shows the new expiration time.
3. When you are finished, select **Delete** for the lab and confirm.

**Expected result:** An extension updates the displayed expiration. Deleting a lab permanently removes its VMs and their work; create a snapshot first if you may need to recover a VM state.

See [Snapshots and Recovery](snapshots.md) before making a risky change.
