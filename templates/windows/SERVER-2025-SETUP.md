# Windows Server 2025 Base Template Setup Guide

This guide documents how to build the **L1 base Windows Server 2025 template**
that the Crucible self-service portal's template wizard then provisions FROM.
The output is a single, blessed `student-windows-server-2025` VM in the
Templates folder, sysprepped with our `windows-unattend.xml` + cloudbase-init
combo, ready for instructors to clone via the wizard.

It follows the **exact same L1/L2/L3 conventions** as the Windows 11 guide
([SETUP.md](SETUP.md)) so the identical wizard/provisioner code drives L2
(monthly/quarterly course templates) and L3 (student pods) for both OSes.
Read SETUP.md first for the hierarchy and cloudbase-init rationale; this doc
only covers where Server 2025 differs.

## Template hierarchy (same as Windows 11)

| Level | Built by | Purpose |
|-------|----------|---------|
| **L1 — Base** | This guide (mostly automated via answer ISO + `govc`) | Vanilla WS2025 + VMware Tools + fully patched + cloudbase-init + unattend.xml, sysprepped. The "blessed" image. |
| **L2 — Course template** | Crucible wizard (instructor self-service) | Full-clones L1, instructor customizes, wizard re-sysprep + snapshots as `base-image`. |
| **L3 — Student pod VM** | Crucible provisioner | Linked clone off L2's `base-image` snapshot, per-pod password injected via guestinfo + cloudbase-init. |

> [!important] Do NOT mark the L1 VM as a template and do NOT snapshot it
> Same rule as Windows 11 — the wizard full-clones the L1 *VM* and creates
> the `base-image` snapshot itself during the L2 generalize step.

## What is different from Windows 11 (summary)

| Topic | Windows 11 | Windows Server 2025 |
|-------|-----------|---------------------|
| OS install | Manual OOBE + BypassNRO | **Fully automated** by `autounattend-server2025.xml` on an answer ISO |
| Licensing | Retail/OEM key | **Evaluation media** — 180 day, rearmable → set **`SkipRearm=1`** so generalize doesn't burn rearms |
| BitLocker | vTPM auto-encrypts → must `manage-bde -off` | **N/A** (server SKU + no vTPM) — verify anyway |
| Build access | Web console (interactive) | **Headless via `govc guest.run`** from k3sv01 (Vault-signed SSH) |
| Windows Update | Settings UI | **Native WUA COM API as a SYSTEM scheduled task** (see Step 4) |
| Pre-sysprep | — | **`DISM StartComponentCleanup` + reboot** before sysprep (avoids sysprep error 1018) |
| guestId | `windows11_64Guest` | `windows2019srvNext_64Guest` |
| ComputerName (build) | random | `WIN-WS2025-L1` |

## Prerequisites

- The two ISOs on `[NAS-BackupsAndISOS]`:
  - `ISOs/Windows/WindowsServer2025.iso` (Datacenter, Desktop Experience, Eval)
  - `ISOs/Windows/ws2025-autounattend.iso` — the **answer ISO** built from
    `templates/windows/autounattend-server2025.xml` (see "Building the answer
    ISO" below). Ideally also bundles VMware Tools `setup64.exe` under
    `\vmtools\` so Tools installs unattended.
- `govc` access to vCenter from `k3sv01` (Vault-signed SSH cert; secret keys
  `vcenter-url` / `vcenter-user` / `vcenter-password`).
- This repo (for `internal/provisioner/assets/windows-unattend.xml` — the
  version-neutral sysprep answer file, identical to the W11 flow).

> [!danger] Vault SSH cert = 2h TTL and long WS2025 builds outrun it
> Windows Update alone can exceed 2h. Re-sign with
> `.\future\scripts\vault-ssh.ps1 --sign-only` from the Homelab repo root
> whenever SSH silently hangs. k3sv01 `/tmp` may be cleared between signings —
> re-`scp` any helper scripts.

## VM hardware (replicate exactly)

- Firmware **EFI**, guestId `windows2019srvNext_64Guest`
- 4 vCPU / 8192 MB RAM
- 60 GB disk on **pvscsi** controller, datastore `[iSCSI-vmstore]`
- **vmxnet3** NIC on `PG-VM-Lab` (VLAN 30 — has DHCP + NAT egress)
- CD1 = `WindowsServer2025.iso`, CD2 = `ws2025-autounattend.iso`, both
  **start-connected** (the vCenter svc account cannot toggle CD connect state
  at runtime, but start-connected CDs attach on power-on)
- **No vTPM** (intentional — avoids auto-BitLocker)

## Step 1: Automated install from the answer ISO

Unlike Windows 11, there is **no manual OOBE**. With both CDs start-connected
and a blank/wiped disk, EFI boots the install ISO directly (no "press any key"
prompt appears when the disk has no bootable OS) and
`autounattend-server2025.xml` drives the entire install:

- wipes disk 0, UEFI/GPT layout, installs **Datacenter (Desktop Experience)**
- enables built-in Administrator with throwaway password `BuildAdmin123!`
- skips OOBE, auto-logons once as Administrator and runs the FirstLogonCommands:
  1. sets Administrator **password-never-expires** (build safety — see gotcha)
  2. silently installs **VMware Tools** from `setup64.exe` if present on a CD

Power on and wait ~10-15 min. Confirm the guest reaches the Administrator
desktop and got an IP on VLAN 30.

> [!warning] Account-lockout gotcha (this bit us — caused a blank password)
> Do NOT hammer `govc guest.run` auth during reboots. Windows locks the
> account after ~10 failed attempts in ~10 min, and a mid-build password
> policy once *blanked* the Administrator password entirely. During any
> reboot, poll **`govc vm.info` `toolsRunningStatus`** (no guest auth) — not
> `guest.run` — until Tools is back up, then resume guest ops. The
> `PasswordNeverExpires` FirstLogonCommand above is the belt-and-suspenders
> fix baked into the answer file.

## Step 2: VMware Tools (fallback if the answer ISO didn't bundle it)

The govc-driven prep **cannot start until VMware Tools is running** (govc guest
ops need it; the tell-tale failure is `File /bin/bash was not found`). If the
FirstLogonCommand installed Tools, skip this step. Otherwise:

1. vCenter → right-click VM → **Guest OS → Install VMware Tools** (mounts the
   tools ISO)
2. In the guest console, run `setup64.exe` from the CD (Typical), reboot
3. Verify in vCenter Summary: "VMware Tools: Running" + guest IP shown

Then confirm from k3sv01: `govc vm.info vm-XXXX` shows an IP and Tools running.

> [!tip] Everything below runs headless via `govc guest.run` (creds
> `Administrator:BuildAdmin123!`). Upload a `.ps1` with `govc guest.upload`
> then run with `govc guest.run powershell -File C:\x.ps1` (the `-File` form —
> inline `-Command` mangles quotes/variables).

## Step 3: Harden the build Administrator

Prevent the password-blank incident for the rest of the build:

```powershell
Set-LocalUser -Name Administrator -PasswordNeverExpires $true
net accounts /maxpwage:unlimited
```

## Step 4: Windows Update (native WUA COM as a SYSTEM task)

> [!danger] Do NOT use PSWindowsUpdate, and do NOT run WUA in the govc session
> `PSWindowsUpdate` hangs forever bootstrapping NuGet/PSGallery on an offline-ish
> box. The WUA COM API also hangs if run directly inside the non-interactive
> Tools session. **Run it as a SYSTEM scheduled task** and poll a status file.

Worker script (`wu-worker.ps1`) — uses `Microsoft.Update.Session`, loops
search → download → install, writes `PASS_DONE`/`REBOOT_REQUIRED` to
`C:\wu-status.txt` and appends to `C:\wu-worker.log`:

```powershell
$status = 'C:\wu-status.txt'; $log = 'C:\wu-worker.log'
function Log($m){ "$(Get-Date -f o) $m" | Out-File $log -Append }
$session = New-Object -ComObject Microsoft.Update.Session
$searcher = $session.CreateUpdateSearcher()
Log "searching"; $r = $searcher.Search("IsInstalled=0 and IsHidden=0")
Log ("found " + $r.Updates.Count)
if ($r.Updates.Count -eq 0){ "PASS_DONE" | Out-File $status; return }
$dl = New-Object -ComObject Microsoft.Update.UpdateColl
foreach($u in $r.Updates){ $u.AcceptEula() | Out-Null; $dl.Add($u) | Out-Null }
$d = $session.CreateUpdateDownloader(); $d.Updates = $dl; $d.Download() | Out-Null
$inst = $session.CreateUpdateInstaller(); $inst.Updates = $dl
$res = $inst.Install()
if ($res.RebootRequired){ "REBOOT_REQUIRED" | Out-File $status } else { "PASS_DONE" | Out-File $status }
```

Register + run it as SYSTEM (so it has an interactive-equivalent WUA context):

```powershell
$a = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument '-NoProfile -ExecutionPolicy Bypass -File C:\wu-worker.ps1'
$p = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
Register-ScheduledTask -TaskName 'WUPass' -Action $a -Principal $p -Force
Start-ScheduledTask -TaskName 'WUPass'
```

Poll `C:\wu-status.txt` from k3sv01 (via a small `govc guest.run` reader).
On `REBOOT_REQUIRED`, reboot (poll `vm.info` toolsRunningStatus to detect
back-up — see Step 1 gotcha), then re-run the task. Repeat until `PASS_DONE`
with nothing new.

> [!note] The perpetually re-offered Defender platform update
> WUA will keep re-offering the Defender platform update every pass. It is
> harmless and its servicing churn was the leading suspect for the earlier
> sysprep 1018. **Leave it uninstalled** — do not loop forever chasing it.
> If an online scan wedges `wuauserv` (~20 min hang), clear it with
> `govc vm.power -reset` and continue.

## Step 5: BitLocker — verify N/A

Server 2025 without a vTPM does not auto-encrypt. Confirm anyway:
`Get-BitLockerVolume` should show `FullyDecrypted` / `ProtectionStatus Off`.
If (unexpectedly) encrypted, `manage-bde -off C:` and wait, exactly as W11
[SETUP.md](SETUP.md) Step 5 — an encrypted base image bricks every clone.

## Step 6: Install & configure cloudbase-init (identical to Windows 11)

1. `Invoke-WebRequest https://www.cloudbase.it/downloads/CloudbaseInitSetup_Stable_x64.msi -OutFile C:\cbinit.msi` (or bundle it if offline)
2. `msiexec /i C:\cbinit.msi /qn` — **do not** run sysprep/shutdown from the MSI
3. Write the two conf files **byte-for-byte** as in W11 [SETUP.md](SETUP.md)
   Step 7 (`cloudbase-init.conf` and `cloudbase-init-unattend.conf`, both
   `username=Student`, `VMwareGuestInfoService`, `UserDataPlugin` +
   `SetHostNamePlugin` + `LocalScriptsPlugin`).
4. Copy `internal/provisioner/assets/windows-unattend.xml` from this repo to
   `C:\Windows\Panther\unattend.xml` (the version-neutral sysprep answer file —
   **do not modify it**; it is shared with the Windows 11 flow).
5. Disable the service so clones re-enable it on first boot:
   `Set-Service cloudbase-init -StartupType Disabled`

## Step 7: SkipRearm (Server-2025-specific, REQUIRED)

Server 2025 is **Evaluation media**. `sysprep /generalize` consumes a rearm,
and the eval SKU has a limited rearm count — without this, monthly/quarterly
L2 re-syspreps would eventually exhaust rearms and break the pipeline. Set:

```powershell
Set-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\SoftwareProtectionPlatform' -Name SkipRearm -Value 1 -Type DWord
```

This makes generalize **not** consume a rearm, enabling unlimited L2 rebuilds.

## Step 8: Final cleanup + component-store finalize (avoids sysprep 1018)

> [!danger] This is the key mitigation for sysprep error **1018**
> (`ERROR_KEY_DELETED` / 0x800703fa). A cumulative update can leave a CBS
> registry key marked-for-deletion; `StartComponentCleanup` finalizes pending
> component operations and clears it. A rebuild that skipped this failed
> sysprep 1018 repeatedly; the rebuild that ran it succeeded first try.

```powershell
# 1. finalize the component store (can take 5-15 min)
DISM /Online /Cleanup-Image /StartComponentCleanup
# (do NOT use /ResetBase unless you accept losing update-uninstall ability)
```

Then **reboot and settle** (uptime ≥ ~150 s, no pending CBS session). Verify a
clean pre-sysprep state — all of these should be clear/expected:

- `build` = current (e.g. `26100.32995` after the 2026-06 cumulative)
- `Get-Service cloudbase-init` StartMode = **Disabled**
- `C:\Windows\Panther\unattend.xml` present
- Administrator `PasswordExpires` = empty (never)
- `SessionsPending\Exclusive` = 0, no `RebootPending`, no
  `PendingFileRenameOperations`
- `SkipRearm` = 1

Finally empty temp + recycle bin **and remove build artifacts** (otherwise
scripts/logs/installers bake into the image and every clone inherits them):

```powershell
Remove-Item C:\*.ps1, C:\wu-*, C:\cbinit.msi -Force -EA SilentlyContinue
Get-ChildItem C:\Windows\Temp,$env:TEMP -Recurse -Force | Remove-Item -Recurse -Force -EA SilentlyContinue
Clear-RecycleBin -Force -EA SilentlyContinue
```

> [!note] Do this BEFORE sysprep. Once generalized, the box must not be
> powered on, so leftover `C:\` build files can no longer be cleaned and will
> persist into every L2/L3 clone (harmless but untidy).

## Step 9: Sysprep ONCE (on a pristine box)

> [!danger] Sysprep exactly once on a clean box
> Every failed/partial generalize corrupts the box further (Run 2 fails with
> "machine is in an invalid state" 0x1f as a *consequence* of a partial Run 1).
> If sysprep fails, **rebuild from the answer ISO** rather than retrying on the
> dirtied box.

Launch as a detached SYSTEM scheduled task (so the govc session dropping at
shutdown doesn't kill it):

```powershell
$a = New-ScheduledTaskAction -Execute 'C:\Windows\System32\Sysprep\sysprep.exe' -Argument '/generalize /oobe /shutdown /unattend:C:\Windows\Panther\unattend.xml'
$p = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
Register-ScheduledTask -TaskName 'SysprepGo' -Action $a -Principal $p -Force
Start-ScheduledTask -TaskName 'SysprepGo'
```

Poll `govc vm.info` power state. **poweredOff = success** (~90 s–8 min). Do
**not** power the VM back on — that starts OOBE and consumes the generalized
state. If it does not power off, read
`C:\Windows\System32\Sysprep\Panther\setuperr.log`.

## Step 10: Detach CDs + register with Crucible

1. In vSphere, **disconnect both CDs** (svc account lacks device.connect;
   this is a manual step). Leave them as devices, just disconnected.
2. Crucible portal → **Admin → Templates → New Template**:
   - **Name:** `student-windows-server-2025` (versioned slug)
   - **OS type:** `windows`
   - **Source type:** `clone_vcenter`
   - **Source ref:** this VM's moref (e.g. `vm-11950`)
3. Submit. Do **not** mark as template or snapshot.

## Step 11: Smoke-test one L2 wizard run

Run the wizard end-to-end against a throwaway course to confirm the shared
pipeline works for WS2025 (verifies `SkipRearm` lets re-sysprep succeed):

1. Template detail → **Run Wizard** → Step 1 provision → `tpl-xxx` appears
2. Step 2 configure: WebMKS console, log in as `Student` / `Changeme123!`
   (bootstrap password from `windows-unattend.xml`), make a trivial change,
   mark complete
3. Step 3 generalize: enter `Student` / `Changeme123!`, wait for power-off +
   `base-image` snapshot → template state `ready`

## Step 12: Backups + docs (repo rule §5)

Add the VM to ghettoVCB on whichever host holds it and record it:

- `esxi1`: `/vmfs/volumes/datastore1-esxi1/ghettoVCB-vms.txt`
- `esxi2`: `/vmfs/volumes/datastore1-esxi2/ghettoVCB-vms.txt`
- Update `current/00-Infrastructure-Overview.md` and
  `archive/VM-Backup-Strategy.md` in the Homelab vault.

## Monthly / quarterly L2 rebuild (the whole point)

Instructors build their sysprepped course images entirely through the **wizard**
against the published `student-windows-server-2025` template — no manual steps.
Because `SkipRearm=1` is baked into L1 and inherited by every full-clone, each
L2 generalize does **not** consume an eval rearm, so monthly/quarterly rebuilds
run indefinitely. To refresh L1 itself with new patches, re-run **Step 4**
(WU) → **Step 8** (StartComponentCleanup + reboot) → **Step 9** (sysprep) on a
freshly rebuilt box; never sysprep an L1 that has already been generalized.

## Troubleshooting

- **Sysprep 1018 (`ERROR_KEY_DELETED`):** Step 8 `StartComponentCleanup` +
  reboot was skipped, or a pending update left a marked-for-deletion CBS key.
  Rebuild from the answer ISO and run Step 8 before sysprep.
- **Sysprep "machine is in an invalid state" (0x1f):** a prior generalize
  partially ran. The box is unrecoverable for sysprep — rebuild.
- **govc `File /bin/bash was not found`:** VMware Tools is not running (Step 2)
  or the guest is mid-reboot.
- **SSH to k3sv01 silently hangs:** Vault cert expired (2h TTL) — re-sign.
- **Account locked / password blank mid-build:** you hammered guest auth during
  a reboot — poll `vm.info` toolsRunningStatus instead (Step 1 gotcha).
- **cloudbase-init / OOBE-on-clones / RDP / per-clone password:** identical to
  Windows 11 — see [SETUP.md](SETUP.md) Troubleshooting.

## Building the answer ISO

`ws2025-autounattend.iso` must contain `autounattend.xml` (a copy of
`templates/windows/autounattend-server2025.xml`) at the ISO root. Optionally
bundle VMware Tools `setup64.exe` under `\vmtools\` so Step 1's FirstLogonCommand
installs Tools with no internet. Build it with any ISO tool, e.g. on Linux:

```bash
mkdir -p iso/vmtools
cp autounattend-server2025.xml iso/autounattend.xml
# optional: cp /path/to/VMware-tools/setup64.exe iso/vmtools/
genisoimage -o ws2025-autounattend.iso -J -r -V AUTOUNATTEND iso/
```

Upload it to `[NAS-BackupsAndISOS] ISOs/Windows/ws2025-autounattend.iso`.
