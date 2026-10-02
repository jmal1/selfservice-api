# Getting Started

Use Crucible to create a VM and work in it.

## Sign in and open your VM

1. Sign in through Authentik.
2. You land on **Single VM**. A VM you already have is listed there.
3. To create one, select **New environment**, choose a template your instructor has made available, give it a name, and deploy it.
4. Wait until the status is active, then open the VM.

**Expected result:** You can see the VM, its status, and the details needed to connect.

## Choose the right next step

- Use [Connecting to Your VM](connecting.md) to open a console or connect over your VPN.
- Use [Snapshots and Recovery](snapshots.md) before a risky change.
- Use [Troubleshooting](troubleshooting.md) for sign-in, VPN, console, and password help.
- Use **Current Operations** to watch provisioning.
- Use **Quotas** to see how much of your vCPU and RAM is in use.

Only work with the VM and addresses shown in your own Crucible account.

If you forget your Authentik password, talk to your instructor — see [Forgot password](troubleshooting.md#forgot-password-or-locked-out-of-authentik).

## Understand your limits

- A new VM expires after 5 days. **Extend** sets its expiration to 5 days from the time you extend it, and it can be extended at most twice.
- The VM suspends after 2 hours without activity. A VM that stays suspended for 5 days is destroyed.
- A standard account has 4 vCPUs, 4 GB of RAM, and one VM. Your instructor can set a different VM limit for your account. Open **Quotas** to see current use.
- The VM keeps its protected original snapshot plus one snapshot that you create.

## Remove a VM

1. Open the VM from **Single VM**.
2. Select **Delete Pod**, then **Confirm Delete**.

**Expected result:** Crucible queues permanent destruction of the VM and its work. The status may show **Marked for destruction** while cleanup runs. Create a snapshot first if you may need to recover a state.
