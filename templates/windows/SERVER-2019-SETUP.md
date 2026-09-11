# Windows Server 2019 Base Template Setup Guide

This guide documents how to build the **L1 base Windows Server 2019 template**
that the Crucible self-service portal's template wizard then provisions FROM.
The output is a single, blessed `student-windows-server-2019` VM in the
Templates folder, sysprepped with our `windows-unattend.xml` + cloudbase-init
combo, ready for instructors to clone via the wizard.

It follows the **exact same L1/L2/L3 conventions** as the gold Windows Server
2022 guide ([SERVER-2022-SETUP.md](SERVER-2022-SETUP.md), July 2026) and the
Windows 11 guide ([SETUP.md](SETUP.md)), so the identical wizard/provisioner
code drives L2 (monthly/quarterly course templates) and L3 (student pods).
**Read SERVER-2022-SETUP.md first** — this doc is a delta and only calls out
where Server 2019 differs from that gold 2022 flow.

> [!important] Do NOT use the deprecated vault PowerCLI scripts
> Homelab vault `future/scripts/build-student-windows-server-*.ps1` is a
> single-level flow that marked a vCenter Template=true VM. That path is
> **deprecated**. Follow this runbook + `templates/windows/` +
> `docs/instructor/os-recipes.md`. Do not invent a new credential protocol:
> the student local account is **`Student`**, and per-clone passwords come
> from existing `clone_with_customize` + Cloudbase-Init (`VMwareGuestInfoService`)
> exactly as 2022.

> [!warning] Server 2025 is BLOCKED — do not rebuild it as a gate
> Microsoft `explorer.exe` `0xc0000409` on first clone boot after sysprep,
> plus a Cloudbase-Init password failure on that SKU. Tracked as a known
> issue on the Homelab vault **[[Roadmap]]**. Server 2022 (July 2026) is gold.
> This 2019 L1 is the additional Server SKU this wave ships.

## Template hierarchy (same as Windows 11 / Server 2022 gold)

| Level | Built by | Purpose |
|-------|----------|---------|
| **L1 — Base** | This guide (mostly automated via answer ISO + `govc`) | Vanilla WS2019 + VMware Tools + fully patched + cloudbase-init + unattend.xml, sysprepped. The "blessed" image. |
| **L2 — Course template** | Crucible wizard (instructor self-service) | Full-clones L1, instructor customizes, wizard re-sysprep + snapshots as `base-image`. |
| **L3 — Student pod VM** | Crucible provisioner | Linked clone off L2's `base-image` snapshot, per-pod password injected via guestinfo + cloudbase-init. |

> [!important] Do NOT mark the L1 VM as a template and do NOT snapshot it
> Same rule as Windows 11 / Server 2022 — the wizard full-clones the L1 *VM*
> and creates the `base-image` snapshot itself during the L2 generalize step.

## What is different from Server 2022 gold (summary)

| Topic | Server 2022 (gold, July 2026) | Server 2019 |
|-------|------------------------------|-------------|
| Edition | Standard (Desktop Experience) | **Standard (Desktop Experience)** — same |
| WIM `/IMAGE/NAME` | `Windows Server 2022 SERVERSTANDARD` | **`Windows Server 2019 SERVERSTANDARD`** (verify on media; not `…CORE`) |
| Disk controller | LSI Logic SAS | **LSI Logic SAS** (same — see gotcha) |
| guestId | `windows2019srvNext_64Guest` | **`windows2019srv_64Guest`** (no `Next`) |
| ComputerName (build) | `WIN-WS2022-L1` | `WIN-WS2019-L1` |
| Install ISO | `WindowsServer2022.iso` | **`WindowsServer2019.iso`** (place on NAS first) |
| Answer ISO / XML | `ws2022-autounattend.iso` / `autounattend-server2022.xml` | **`ws2019-autounattend.iso` / `autounattend-server2019.xml` |
| pvscsi folder (if you ever inject) | Tools `pvscsi\Win10\amd64` | Tools **`pvscsi\Win8\amd64`** |
| Everything else | — | **Identical** (answer ISO → WU as SYSTEM task → Cloudbase-Init `username=Student` → shared `windows-unattend.xml` → `SkipRearm=1` → `sysprep /generalize /oobe /shutdown` → register L1 for `clone_with_customize`) |

> [!danger] Disk controller: use **LSI Logic SAS**, not pvscsi
> Same as 2022. WS2019's installer WinPE does **not** reliably include the
> VMware pvscsi driver — a pvscsi boot disk is invisible to Setup ("we
> couldn't find any drives"). Put the L1 disk on **`VirtualLsiLogicSAS`**.
> If you insist on pvscsi, inject `pvscsi.inf` from Tools
> `…\pvscsi\Win8\amd64` (not Win10) via
> `<PnpCustomizationsWinPE><DriverPaths>` — not covered here.

## Prerequisites

- The two ISOs on `[NAS-BackupsAndISOS]`:
  - `ISOs/Windows/WindowsServer2019.iso` (Standard, Desktop Experience, Eval).
    **Not yet in this lab's ISO library** — a human must download Evaluation
    Center / VLSC media and place it at that path with that filename before
    this L1 can be built.
  - `ISOs/Windows/ws2019-autounattend.iso` — the **answer ISO** built from
    `templates/windows/autounattend-server2019.xml` (see "Building the answer
    ISO" below). Ideally also bundles VMware Tools `setup64.exe` / `setup.exe`
    under `\vmtools\` so Tools installs unattended.
- `govc` access to vCenter from `k3sv01` (Vault-signed SSH cert; secret keys
  `vcenter-url` / `vcenter-user` / `vcenter-password`).
- This repo (for `internal/provisioner/assets/windows-unattend.xml` — the
  version-neutral sysprep answer file, identical to the W11/2022 flow).

> [!important] VERIFY the WIM edition name before building
> The answer file selects the edition by `/IMAGE/NAME`. Confirm the exact name
> the media uses for **Standard Desktop Experience** before you build the ISO:
> mount `WindowsServer2019.iso` and run
> `dism /Get-WimInfo /WimFile:<drive>\sources\install.wim`. Standard Desktop
> Experience is typically index 2, name `Windows Server 2019 SERVERSTANDARD`.
> If the media labels it differently, update the `<Value>` in
> `autounattend-server2019.xml` and rebuild the answer ISO. Also confirm the
> media is **Evaluation** (drives the `SkipRearm` step) — GA/volume media with
> a key needs the ProductKey / `SkipRearm` handling adjusted. Do **not** pick
> `SERVERSTANDARDCORE`.

> [!danger] Vault SSH cert = 2h TTL and long WS2019 builds outrun it
> Windows Update alone can exceed 2h. Re-sign with
> `.\future\scripts\vault-ssh.ps1 --sign-only` from the Homelab repo root
> whenever SSH silently hangs. k3sv01 `/tmp` may be cleared between signings —
> re-`scp` any helper scripts.

## VM hardware (replicate exactly)

- Firmware **EFI**, guestId **`windows2019srv_64Guest`**
- 4 vCPU / 8192 MB RAM
- 60 GB disk on **LSI Logic SAS** (`VirtualLsiLogicSAS`) controller, datastore
  `[iSCSI-vmstore]`
- **vmxnet3** NIC on `PG-VM-Lab` (VLAN 30 — has DHCP + NAT egress)
- CD1 = `WindowsServer2019.iso`, CD2 = `ws2019-autounattend.iso`, both
  **start-connected** (the vCenter svc account cannot toggle CD connect state
  at runtime, but start-connected CDs attach on power-on)
- **No vTPM** (intentional — avoids auto-BitLocker on the server SKU)
- Build on **esxi1** (esxi2 has documented NAS write flakiness; `iSCSI-vmstore`
  avoids NFS but keep clone-heavy ops on esxi1 anyway)

## Step 1: Automated install from the answer ISO

Unlike Windows 11, there is **no manual OOBE**. With both CDs start-connected
and a blank/wiped disk, EFI boots the install ISO directly (no "press any key"
prompt appears when the disk has no bootable OS) and
`autounattend-server2019.xml` drives the entire install:

- wipes disk 0, UEFI/GPT layout, installs **Standard (Desktop Experience)**
- enables built-in Administrator with throwaway password `REPLACE_WITH_BUILD_PASSWORD`
- skips OOBE, auto-logons once as Administrator and runs the FirstLogonCommands:
  1. sets Administrator **password-never-expires** (build safety — see gotcha)
  2. silently installs **VMware Tools** from `setup64.exe` / `setup.exe` if
     present on a CD

Power on and wait ~10-15 min. Confirm the guest reaches the Administrator
desktop and got an IP on VLAN 30.

> [!warning] Account-lockout gotcha (this bit us on 2022 — caused a blank password)
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
2. In the guest console, run `setup64.exe` (or `setup.exe` on newer Tools
   media) from the CD (Typical), reboot
3. Verify in vCenter Summary: "VMware Tools: Running" + guest IP shown

Then confirm from k3sv01: `govc vm.info vm-XXXX` shows an IP and Tools running.

> [!tip] Everything below runs headless via `govc guest.run` (creds
> `Administrator:REPLACE_WITH_BUILD_PASSWORD`). Upload a `.ps1` with `govc guest.upload`
> then run with `govc guest.run powershell -File C:\x.ps1` (the `-File` form —
> inline `-Command` mangles quotes/variables).

## Step 3: Harden the build Administrator

Prevent the password-blank incident for the rest of the build:

```powershell
Set-LocalUser -Name Administrator -PasswordNeverExpires $true
net accounts /maxpwage:unlimited
```

## Step 4: Windows Update (native WUA COM as a SYSTEM task)

Identical to Server 2022 gold. Do **not** use PSWindowsUpdate, and do **not**
run WUA in the govc session.

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
> harmless. **Leave it uninstalled** — do not loop forever chasing it.
> If an online scan wedges `wuauserv` (~20 min hang), clear it with
> `govc vm.power -reset` and continue.

## Step 5: BitLocker — verify N/A

Server 2019 without a vTPM does not auto-encrypt. A stock **SERVERSTANDARD
(Desktop Experience)** eval install typically does **not** include the
BitLocker optional feature, so `Get-BitLockerVolume` may be *not present*
and `manage-bde -status C:` reports no BitLocker — both are the expected
clean state (same as 2022 gold). Confirm with:
`manage-bde -status C:` → no conversion/protection lines.
If (unexpectedly) encrypted, `manage-bde -off C:` and wait, exactly as W11
[SETUP.md](SETUP.md) Step 5 — an encrypted base image bricks every clone.

## Step 6: Install & configure cloudbase-init (identical to Windows 11 / 2022)

1. `Invoke-WebRequest https://www.cloudbase.it/downloads/CloudbaseInitSetup_Stable_x64.msi -OutFile C:\cbinit.msi` (or bundle it if offline)
2. `msiexec /i C:\cbinit.msi /qn` — **do not** run sysprep/shutdown from the MSI
3. Write the two conf files **byte-for-byte** as in W11 [SETUP.md](SETUP.md)
   Step 7 (`cloudbase-init.conf` and `cloudbase-init-unattend.conf`, both
   `username=Student`, `VMwareGuestInfoService`, `UserDataPlugin` +
   `SetHostNamePlugin` + `LocalScriptsPlugin`).
4. Copy `internal/provisioner/assets/windows-unattend.xml` from this repo to
   `C:\Windows\Panther\unattend.xml` (the version-neutral sysprep answer file —
   **do not modify it**; it is shared with the Windows 11 / 2022 flow).
5. Disable the service so clones re-enable it on first boot:
   `Set-Service cloudbase-init -StartupType Disabled`

This is the existing `clone_with_customize` / Cloudbase-Init contract. Do not
add a new password protocol.

## Step 7: SkipRearm (Evaluation media, REQUIRED)

Server 2019 Eval media is **180-day, rearmable**. `sysprep /generalize`
consumes a rearm, and the eval SKU has a limited rearm count — without this,
monthly/quarterly L2 re-syspreps would eventually exhaust rearms and break the
pipeline. Set (same as 2022 gold):

```powershell
Set-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\SoftwareProtectionPlatform' -Name SkipRearm -Value 1 -Type DWord
```

This makes generalize **not** consume a rearm, enabling unlimited L2 rebuilds.
(If the media turns out to be GA/volume-licensed rather than eval, this is
harmless but unnecessary.)

## Step 8: Final cleanup + component-store finalize (avoids sysprep 1018)

> [!danger] This is the key mitigation for sysprep error **1018**
> (`ERROR_KEY_DELETED` / 0x800703fa). Same as 2022 gold. A cumulative update
> can leave a CBS registry key marked-for-deletion; `StartComponentCleanup`
> finalizes pending component operations and clears it.

```powershell
# 1. finalize the component store (can take 5-15 min)
DISM /Online /Cleanup-Image /StartComponentCleanup
# (do NOT use /ResetBase unless you accept losing update-uninstall ability)
```

Then **reboot and settle** (uptime ≥ ~150 s, no pending CBS session). Verify a
clean pre-sysprep state — all of these should be clear/expected:

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

1. **Detach both CDs.** The svc account lacks `device.connect` (runtime
   connect toggle) but **can eject** — `govc device.cdrom.eject -vm <VM>
   -device cdrom-3000` / `cdrom-3001` removes the ISO backing and leaves the
   device disconnected (empty ATAPI). No manual UI step needed.
2. Register via the Crucible portal **Admin → Templates → New Template** (or an
   equivalent DB insert — `AdminCreateTemplate` is a plain insert + folder-cache
   invalidation, no side effects). **Mirror the live gold `Windows Server 2022`
   row** — that template uses `source_type = manual` with the **VM name** in
   `vcenter_template` (the L1 is a vCenter clone source for
   `clone_with_customize`; do **not** invent a moref-only `clone_vcenter`
   registration unless you are matching a different live row on purpose):
   - **Name:** `Windows Server 2019`
   - **OS type:** `windows`
   - **kind:** `clone_with_customize`
   - **Source type:** `manual`
   - **vcenter_template:** `student-windows-server-2019` (the VM *name*; the
     wizard/provisioner resolves the source VM by name for manual templates)
   - **vcenter_vm_id / source_ref:** empty
   - **Defaults:** 4 vCPU / 4096 MB / 60 GB, `Student` / `REPLACE_WITH_BUILD_PASSWORD`,
     staging_network `PG-VM-Lab`, `assign_ip=true`, `is_active=true`,
     `template_state=active`, `is_internal=false`
3. If you inserted directly, `kubectl -n selfservice rollout restart
   deployment/selfservice-api` to refresh the templates folder cache. Do **not**
   mark the VM as a vSphere template or snapshot it (the wizard snapshots
   `base-image` at L2).

Per-clone student passwords stay on the existing `clone_with_customize` +
Cloudbase-Init path. No new credential protocol.

## Step 11: Smoke-test one L2 wizard run

Run the wizard end-to-end against a throwaway course to confirm the shared
pipeline works for WS2019 (verifies `SkipRearm` lets re-sysprep succeed):

1. Template detail → **Run Wizard** → Step 1 provision → `tpl-xxx` appears
2. Step 2 configure: WebMKS console, log in as `Student` / `REPLACE_WITH_BUILD_PASSWORD`
   (bootstrap password from `windows-unattend.xml`), make a trivial change,
   mark complete
3. Step 3 generalize: enter `Student` / `REPLACE_WITH_BUILD_PASSWORD`, wait for power-off +
   `base-image` snapshot → template state `ready`

## Step 12: Backups + docs

> [!note] L1 gold-image templates are **excluded** from ghettoVCB by convention
> Same as `student-windows-server-2022` / `student-windows-11`: do **not** add
> `student-windows-server-2019` to ghettoVCB. The image is reproducible from
> this runbook + the answer ISO, and Crucible snapshots handle L2.

## Monthly / quarterly L2 rebuild (the whole point)

Instructors build their sysprepped course images entirely through the **wizard**
against the published `student-windows-server-2019` template — no manual steps.
Because `SkipRearm=1` is baked into L1 and inherited by every full-clone, each
L2 generalize does **not** consume an eval rearm, so monthly/quarterly rebuilds
run indefinitely. To refresh L1 itself with new patches, re-run **Step 4**
(WU) → **Step 8** (StartComponentCleanup + reboot) → **Step 9** (sysprep) on a
freshly rebuilt box; never sysprep an L1 that has already been generalized.

## Troubleshooting

- **Setup "we couldn't find any drives" / no install target:** the boot disk is
  on pvscsi and WS2019 WinPE lacks the driver — rebuild the VM with an
  **LSI Logic SAS** controller (see the disk-controller gotcha). If injecting
  pvscsi instead, use Tools `pvscsi\Win8\amd64`, not Win10.
- **Sysprep 1018 (`ERROR_KEY_DELETED`):** Step 8 `StartComponentCleanup` +
  reboot was skipped, or a pending update left a marked-for-deletion CBS key.
  Rebuild from the answer ISO and run Step 8 before sysprep.
- **Sysprep "machine is in an invalid state" (0x1f):** a prior generalize
  partially ran. The box is unrecoverable for sysprep — rebuild.
- **Install stops on the edition picker:** the `/IMAGE/NAME` in
  `autounattend-server2019.xml` doesn't match the media — verify with
  `dism /Get-WimInfo` and correct it, then rebuild the answer ISO.
- **govc `File /bin/bash was not found`:** VMware Tools is not running (Step 2)
  or the guest is mid-reboot.
- **SSH to k3sv01 silently hangs:** Vault cert expired (2h TTL) — re-sign.
- **Account locked / password blank mid-build:** you hammered guest auth during
  a reboot — poll `vm.info` toolsRunningStatus instead (Step 1 gotcha).
- **cloudbase-init / OOBE-on-clones / RDP / per-clone password:** identical to
  Windows 11 / Server 2022 gold — see [SETUP.md](SETUP.md) Troubleshooting and
  [SERVER-2022-SETUP.md](SERVER-2022-SETUP.md).
- **`template_verify` fails "Student never switched to its generated password …
  cloudbase-init likely isn't running" (Server SKUs):** Windows **Server**
  silently SKIPS the unattend `FirstLogonCommands` when `SkipMachineOOBE`/
  `SkipUserOOBE` are set (Windows 11 client still runs them), so cloudbase-init
  was never re-enabled on the clone. Fixed centrally: the shared
  `internal/provisioner/assets/windows-unattend.xml` now enables cloudbase-init
  in the **`specialize` pass** (`RunSynchronousCommand` →
  `sc config cloudbase-init start= delayed-auto`), which runs on all SKUs. If
  you see this, make sure the provision-worker is running an image built after
  that fix. This is **not** the Server 2025 Cloudbase password known issue
  (Homelab vault **[[Roadmap]]**); do not "fix" 2019 by rebuilding 2025.

## Building the answer ISO

`ws2019-autounattend.iso` must contain `autounattend.xml` (a copy of
`templates/windows/autounattend-server2019.xml`) at the ISO root. Optionally
bundle VMware Tools `setup64.exe` (or `setup.exe`) under `\vmtools\` so Step 1's
FirstLogonCommand installs Tools with no internet. Build it with any ISO tool,
e.g. on Linux:

```bash
mkdir -p iso/vmtools
cp autounattend-server2019.xml iso/autounattend.xml
# optional: cp /path/to/VMware-tools/setup64.exe iso/vmtools/
genisoimage -o ws2019-autounattend.iso -J -r -V AUTOUNATTEND iso/
```

Or on Windows with the Windows ADK's `oscdimg`:

```cmd
copy autounattend-server2019.xml iso\autounattend.xml
oscdimg -j1 -o -m -lAUTOUNATTEND iso ws2019-autounattend.iso
```

Upload it to `[NAS-BackupsAndISOS] ISOs/Windows/ws2019-autounattend.iso`
(e.g. `govc datastore.upload -ds=NAS-BackupsAndISOS ws2019-autounattend.iso ISOs/Windows/ws2019-autounattend.iso`).

## Human lab checklist (ISO + L1)

A human on lab does this after merging this docs PR. This repo does **not**
claim the L1 was built or runtime-verified.

1. Place **`WindowsServer2019.iso`** at
   `[NAS-BackupsAndISOS] ISOs/Windows/WindowsServer2019.iso`
   (Microsoft Evaluation Center / VLSC; Standard Desktop Experience Eval).
2. Build and upload **`ws2019-autounattend.iso`** as above.
3. Create the VM with the hardware in "VM hardware", attach both CDs
   start-connected, power on.
4. Wait for unattended Setup + Tools (Steps 1–2).
5. Harden Administrator → Windows Update as SYSTEM task until `PASS_DONE`
   (Steps 3–4).
6. Verify BitLocker N/A (Step 5).
7. Install Cloudbase-Init, write conf (`username=Student`), copy shared
   `windows-unattend.xml`, disable the service (Step 6).
8. `SkipRearm=1` (Step 7).
9. `DISM /StartComponentCleanup`, reboot, settle, clean `C:\` (Step 8).
10. `sysprep /generalize /oobe /shutdown` once via the SYSTEM task (Step 9).
    Leave powered off — do not snapshot L1.
11. Eject CDs. Register `student-windows-server-2019` as
    `clone_with_customize` / `source_type=manual` (Step 10).
12. Smoke-test one L2 wizard run until `base-image` exists (Step 11).
