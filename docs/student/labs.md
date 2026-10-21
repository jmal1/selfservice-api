# Creating and Managing Labs

This page is only for accounts that have been granted labs. Create a lab from the templates or blueprints your instructor has shared with you.

## Create a lab

1. Open **My Labs** and select **New lab**.
2. Pick a template for one VM or a blueprint for a prepared multi-VM lab.
3. Review the visible name and description, enter a lab name, and select **Deploy**.
4. Watch the deployment progress, then select **Open** when the lab becomes active.

**Expected result:** Your lab appears in **My Labs** with one or more VM cards.

## Understand your limits

- A new lab expires after 5 days. **Extend** sets its expiration to 5 days from the time you extend it, and each lab can be extended at most twice. Extend appears on the list when the lab is close to expiring; it is always available after you open the lab.
- Labs suspend after 2 hours without activity. A lab that remains suspended for 5 days is destroyed.
- A standard account with labs has 2 labs, 4 vCPUs, and 4 GB of RAM. Open **Quotas** to see current use.
- Each VM keeps its protected original snapshot plus one snapshot that you create.

## Manage a VM

1. Open your lab and select the VM you want to manage.
2. Use the available power action: **Start**, **Stop**, **Restart**, or **Reset**.
3. Wait for the VM status to update before connecting again. A reset is a hard restart and can lose unsaved work.

**Expected result:** The selected VM reaches the requested power state.

## Read your lab briefing

If your instructor wrote a briefing for the lab, it appears in a **Briefing** card on the lab page after the lab is created. The same applies to a Single VM. A lab built from a blueprint can show several tabs: **Lab** for the whole exercise and one tab for each kind of VM.

1. Select **Expand** to read the whole briefing on the lab page.
2. Select **Open in new window** to keep the briefing beside the VM console. Selecting it again brings the same window forward. If your browser blocks the popup, select **Open in new tab** instead.
3. Select **Copy** on a command block to copy it, then paste it into the VM console.

**Expected result:** You can read the briefing while you work in the VM. Your instructor can update it during the lab; reload the page to see the latest version.

## Watch expiration and remove a lab

1. Check the expiration time shown on your lab.
2. If your active lab needs more time, select **Extend** and confirm the request. Crucible shows the new expiration time.
3. When you are finished, open the lab and select **Delete Pod**, then confirm. Deleting is not available from **My Labs**.

**Expected result:** An extension updates the displayed expiration. Deleting a lab queues permanent destruction of its VMs and their work, so its status may show **Marked for destruction** while cleanup runs. Create a snapshot first if you may need to recover a VM state.

See [Snapshots and Recovery](snapshots.md) before making a risky change, and [Vulnerability Assessments](testing.md) when the lab includes assessments.
