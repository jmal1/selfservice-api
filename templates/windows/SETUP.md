# Windows 11 Base Template Setup Guide

This guide documents how to build the **L1 base Windows 11 template** that
the Crucible self-service portal's template wizard then provisions FROM.
The output of this guide is a single, blessed `student-windows-11` VM
that lives in the Templates folder, has been sysprepped with our
unattend.xml + cloudbase-init combo, and is ready for instructors to
clone via the wizard.

## Template hierarchy (read this first)

The platform uses a 3-level template hierarchy:

| Level | Built by | Purpose |
|-------|----------|---------|
| **L1 — Base** | This guide (manual, one-time per OS) | Vanilla Windows + VMware Tools + cloudbase-init + unattend.xml, sysprepped. The "blessed" image. |
| **L2 — Course template** | Crucible wizard (instructor self-service) | Full-clones L1, instructor customizes (installs tools, sets policies), wizard re-sysprep + snapshots as `base-image`. |
| **L3 — Student pod VM** | Crucible provisioner | Linked clone off L2's `base-image` snapshot, per-pod password injected via guestinfo + cloudbase-init. |

> [!important] Stop and read this if you've used the OLD scripts
> The old `build-student-windows-*.ps1` PowerCLI scripts in `future/scripts/`
> are a single-level flow that built a vCenter "template" (Template=true)
> directly. That flow is **deprecated** for the wizard-driven hierarchy
> above. Do NOT mark the L1 VM as a template — leave it as a regular VM
> so the wizard can full-clone it.

## Prerequisites

- Access to vCenter Web Client and an account with VM-modify rights
- Windows 11 Pro ISO mounted on the VM (currently in OOBE state)
- This repo cloned (you'll need `internal/provisioner/assets/windows-unattend.xml`)
- A port group with internet egress for the build phase (the lab uses
  `Mgmt-VLAN10` or `PG-VM-Lab` (VLAN 30) — both have NAT to the internet via
  the home gateway)

## Step 1: Complete OOBE with a throwaway admin

1. Open vCenter → Navigate to the L1 template VM (e.g. `template-windows-11`)
2. Power it on and open the **Web Console** (or VMRC if you have it)
3. At the "Let's connect you to a network" screen, press **Shift+F10**
   to open cmd, then run **one** of these to bypass the Microsoft-account
   requirement:
   - 23H2 and earlier: `oobe\BypassNRO.cmd` (auto-reboots)
   - 24H2: `reg add HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\OOBE /v BypassNRO /t REG_DWORD /d 1 /f` then `shutdown /r /t 0`
   - 24H2 fallback: `start ms-cxh:localonly` (opens local-account dialog directly)
4. After reboot, choose **"I don't have internet"** → **"Continue with limited setup"**
5. Create a throwaway local admin account. Recommended: `crucible` /
   `wizard1234`. **This account is deleted in Step 8** — do not reuse
   the password anywhere else.

## Step 2: Attach the build network

The VM needs internet access to download cloudbase-init and (optionally)
apply Windows Updates. From vCenter:

1. Edit Settings → Network adapter 1 → set to `Mgmt-VLAN10` (or
   `PG-VM-Lab` (VLAN 30) — either works)
2. Apply
3. In the guest, verify with `ping 8.8.8.8` — should respond. If not,
   confirm the adapter shows "Connected" in vCenter and that the guest
   got a DHCP lease (`ipconfig /all`)

> [!tip] PowerCLI alternative
> `Get-VM template-windows-11 | Get-NetworkAdapter | Set-NetworkAdapter -NetworkName "Mgmt-VLAN10" -Confirm:$false`

## Step 3: Install VMware Tools

1. In vCenter, right-click the VM → Guest OS → **Install VMware Tools**
2. In the guest, open File Explorer → CD drive → run `setup64.exe`
3. Accept the defaults (Typical install)
4. Reboot when prompted
5. After reboot, verify in vCenter Summary tab — should now show the
   guest IP and "VMware Tools: Running, version current"

## Step 4: Apply Windows Updates

Cycle through Windows Update until clean. This prevents every student
clone from spending its first 30 minutes downloading 6 months of
patches.

1. Settings → Windows Update → Check for updates
2. Install everything offered, including optional / driver updates
3. Reboot
4. Repeat until "You're up to date" with no pending reboot

> [!note] Skip this step at your discretion if you're rebuilding quickly
> and accept that clones will patch on first boot. Slower student
> first-boot but faster L1 build.

## Step 5: Disable BitLocker

Windows 11 with a vTPM auto-enables BitLocker on the system drive. If
left enabled, sysprep will bake an encrypted volume into the base image
and **every clone will fail to boot** (the keys don't survive sysprep).

From an elevated PowerShell:

```powershell
manage-bde -off C:
# Poll until decryption is complete (can take 5-15 minutes)
while ((Get-BitLockerVolume -MountPoint "C:").VolumeStatus -ne "FullyDecrypted") {
    Get-BitLockerVolume -MountPoint "C:" | Format-Table MountPoint, EncryptionPercentage, VolumeStatus
    Start-Sleep -Seconds 30
}
```

Verify with `Get-BitLockerVolume` — `VolumeStatus` should be
`FullyDecrypted` and `ProtectionStatus` should be `Off`.

## Step 6: Install Cloudbase-Init

1. Download the MSI installer:
   `Invoke-WebRequest -Uri https://www.cloudbase.it/downloads/CloudbaseInitSetup_Stable_x64.msi -OutFile C:\cbinit.msi`
2. Run the installer (`msiexec /i C:\cbinit.msi` or double-click):
   - **Username:** `Student` (capital S — must match unattend.xml exactly)
   - **Serial port:** leave default
   - On the final page, **UNCHECK "Run Sysprep"** — we'll do that manually
   - **UNCHECK "Shutdown"**
3. Click Finish

## Step 7: Configure Cloudbase-Init

Edit `C:\Program Files\Cloudbase Solutions\Cloudbase-Init\conf\cloudbase-init.conf`:

```ini
[DEFAULT]
username=Student
groups=Administrators
inject_user_password=true
first_logon_behaviour=no
metadata_services=cloudbaseinit.metadata.services.vmwareguestinfoservice.VMwareGuestInfoService
plugins=cloudbaseinit.plugins.common.userdata.UserDataPlugin,cloudbaseinit.plugins.common.sethostname.SetHostNamePlugin,cloudbaseinit.plugins.common.localscripts.LocalScriptsPlugin
allow_reboot=false
stop_service_on_exit=false
check_latest_version=false
```

Edit `C:\Program Files\Cloudbase Solutions\Cloudbase-Init\conf\cloudbase-init-unattend.conf`:

```ini
[DEFAULT]
username=Student
groups=Administrators
inject_user_password=true
metadata_services=cloudbaseinit.metadata.services.vmwareguestinfoservice.VMwareGuestInfoService
plugins=cloudbaseinit.plugins.common.sethostname.SetHostNamePlugin
allow_reboot=false
stop_service_on_exit=false
check_latest_version=false
```

Copy `internal/provisioner/assets/windows-unattend.xml` from this repo to `C:\Windows\Panther\unattend.xml`
on the VM (the sysprep command in Step 9 reads it from there).

Disable the cloudbase-init service pre-sysprep — the unattend.xml's
FirstLogonCommands will re-enable + start it on the first boot of every
clone:

```powershell
Set-Service cloudbase-init -StartupType Disabled
```

## Step 8: Delete the throwaway admin (chicken-and-egg dance)

You can't delete the account you're currently logged in as, so:

1. While logged in as `crucible`, open an elevated cmd or PowerShell:
   ```cmd
   net user Administrator AStrongTempPassword123!
   net user Administrator /active:yes
   ```
2. Sign out (Start → user icon → Sign out)
3. Sign in as `.\Administrator` with `AStrongTempPassword123!`
4. Delete the `crucible` account and its profile:
   ```cmd
   net user crucible /delete
   rmdir /s /q C:\Users\crucible
   ```
5. Re-disable the built-in Administrator (sysprep re-disables it anyway,
   but explicit is better):
   ```cmd
   net user Administrator /active:no
   ```

After this step, the only local accounts are:
- `Administrator` (disabled — won't carry into clones in a useful way)
- The `Student` account that unattend.xml will create on first boot of
  every clone

## Step 9: Final Cleanup

While still logged in as Administrator:

1. Empty Recycle Bin (`Clear-RecycleBin -Force`)
2. Clear browser history / temp files
3. Optional: disk cleanup (`cleanmgr /sageset:0` → tick all → `cleanmgr /sagerun:0`)
4. Remove the build network if you want — leave it on `Mgmt-VLAN10` for
   now; the wizard's `CloneTemplateSourceVM` will move clones to the
   staging network anyway.

## Step 10: Sysprep

Run from an **elevated** command prompt:

```cmd
C:\Windows\System32\Sysprep\sysprep.exe /generalize /oobe /shutdown /unattend:C:\Windows\Panther\unattend.xml
```

**Wait for the VM to shut down completely.** Do not interrupt sysprep.
Typical duration: 3-8 minutes. If sysprep fails, check
`C:\Windows\System32\Sysprep\Panther\setuperr.log` for the cause.

## Step 11: Register with Crucible

> [!warning] Do NOT mark as template, do NOT take a snapshot
> The Crucible template wizard handles snapshotting (it creates the
> `base-image` snapshot in the L2 generalize step). Leave the L1 VM as
> a regular VM (`Config.Template=false`) and snapshot-free.

1. In the Crucible portal, go to **Admin → Templates → New Template**
2. Fill in:
   - **Name:** e.g. `student-windows-11-v2` (versioned slug)
   - **OS type:** `windows`
   - **Source type:** `clone_vcenter`
   - **Source ref:** the moref of this VM (e.g. `vm-12345`) —
     find via `(Get-VM template-windows-11).ExtensionData.MoRef.Value`
3. Submit. The new template row enters the `not_started` state.

Then run the wizard end-to-end against a test course to verify L2
provision + generalize + base-image snapshot works:

1. From the template detail page, click **Run Wizard**
2. Wizard Step 1 (provision): wait for `tpl-xxx-xxxxxx` to appear in
   the Templates folder
3. Wizard Step 2 (configure): open the WebMKS console, log in as
   `Student` / `Changeme123!` (the bootstrap password from unattend.xml).
   Make a trivial change so you know it took (e.g. drop a file on the
   desktop). Mark configure complete.
4. Wizard Step 3 (generalize): enter `Student` / `Changeme123!` for the
   guest creds. Wait for the VM to power off + `base-image` snapshot
   to appear.
5. Template state should transition to `ready`.

## Step 12: Smoke test a student clone

1. Provision a test pod against this template via the portal
2. The provisioner should:
   - Linked-clone from the `base-image` snapshot (~30 seconds)
   - Inject `guestinfo.userdata` with a PowerShell script that calls
     `Set-LocalUser -Name Student -Password $unique`
   - Power on the VM
3. After boot:
   - OOBE should be skipped (unattend.xml took effect)
   - `Student` account auto-logs in once
   - Cloudbase-init runs, reads guestinfo, sets per-pod password
   - RDP is enabled and accessible on the pod network

## Troubleshooting

- **Cloudbase-init logs:** `C:\Program Files\Cloudbase Solutions\Cloudbase-Init\log\cloudbase-init.log`
- **Check guestinfo was injected:** vCenter → VM → Configure → Advanced
  → look for `guestinfo.userdata` (base64-encoded)
- **OOBE still showing on clones:** unattend.xml wasn't found —
  verify it's at `C:\Windows\Panther\unattend.xml` and that sysprep was
  invoked with `/unattend:` pointing at it
- **RDP not working:** `netsh advfirewall firewall show rule name="Remote Desktop*"`
- **Per-clone password not set:** check cloudbase-init log for errors
  reading VMwareGuestInfoService; confirm cloudbase-init service started
  on first boot (`Get-Service cloudbase-init`)
- **Clones fail to boot with "Operating system not found":** BitLocker
  was not disabled before sysprep (see Step 5). The base image is
  encrypted and clones can't unlock it. Rebuild from scratch.
- **Sysprep fails with "package was installed for a user, but not provisioned":**
  remove the offending appx packages with
  `Get-AppxPackage -AllUsers <PackageName> | Remove-AppxPackage -AllUsers`,
  then re-run sysprep.

## Notes on hardcoded values in unattend.xml

- **`<TimeZone>Eastern Standard Time</TimeZone>`** — change this if your
  lab is not in US-Eastern. Cloudbase-init does NOT override timezone.
- **`<ComputerName>*</ComputerName>`** — random name on first boot;
  cloudbase-init's `SetHostNamePlugin` then overrides it to the
  per-pod VM name from `guestinfo.metadata`.
- **`Changeme123!`** — bootstrap password for the `Student` account.
  Exists only on the source/L2 templates and for ~30 seconds on each
  clone before cloudbase-init overwrites it. Never persists to a
  student-facing VM.
- **`<UserAuthentication>0</UserAuthentication>`** (RDP NLA off) —
  intentional so first-boot RDP works before the per-clone password
  is set. Treat clones as not-network-exposed until cloudbase-init
  completes (~60 seconds after boot).

