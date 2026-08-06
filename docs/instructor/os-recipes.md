# Per-OS Template Build Recipes

> [!note]
> This page gives you a **copy-me recipe for one operating system at a time**.
> It does **not** re-explain the wizard — read [Building a Template](templates.md)
> first for the full Draft → Provision → Configure → Generalize → Publish
> walkthrough, the standard build login, and the Linux template contract. This
> page only adds the OS-specific values and gotchas that page tells you to look
> up here.

New here? You do **not** need to understand VMware. Find the section for the OS
you want, open [Building a Template](templates.md) in another tab, and whenever
the wizard asks for a value, copy it from the recipe table. Every recipe ends
with **"You're done when…"** so you know it worked.

---

## Before you start — words this page uses

- **Wizard** — the **Admin → Templates → New** form. All the clicking happens
  here and on the template page it creates.
- **ISO** — the OS installer disc file (ends in `.iso`). You pick one from a
  dropdown; you never type a path. ISOs live on the **NAS-BackupsAndISOS**
  storage; if yours is missing, upload it on **Admin → Images** first (see
  [Getting an ISO into the picker](templates.md#getting-an-iso-into-the-picker)).
- **Install mode** — how the OS installs itself. The wizard field is
  **Install mode**; the four choices are `manual`, `cloudinit_cidata`,
  `debian_preseed`, `windows_autounattend`.
- **Guest OS ID** — a short code (like `ubuntu64Guest`) that tells VMware which
  operating system it's building. The wizard field is **Guest OS ID**. Copy the
  exact code from the recipe; a wrong one can make the installer misbehave.
- **Generalize** — the wizard button that cleans the finished VM so every
  student gets a unique copy. What "clean" means differs per OS; each recipe
  says which method applies.
- **The Linux template contract** — the five things a Linux image must have so
  each student's random password actually works. Full details:
  [Linux template contract](templates.md#linux-template-contract). Some recipes
  satisfy it for you; some make you do it by hand.

> [!important] The standard build login is `Student` / `Changeme123!`.
> Use it every time a recipe or the wizard asks you to make up a password. This
> is **not** the password students get — Crucible generates a fresh random one
> per student. See
> [The standard build login](templates.md#the-standard-build-login-studentchangeme123).

---

## Which templates exist / are supported today

Crucible can build a template from three kinds of source (the **Source type**
field in the wizard):

| Source type | When you'd use it | Covered on this page? |
|-------------|-------------------|-----------------------|
| **Clone an existing Crucible template** | Fastest, safest — start from a template that already works | No — see [templates.md](templates.md); no OS install needed |
| **Clone an existing vCenter VM** | Build from an imported OVA or a VM an admin points you at | No — no OS install needed |
| **ISO install** | Install an OS from scratch off an installer disc | **Yes — this whole page** |

The nine OSes below are the ones the ISO-install path is known to handle. The
**install mode** and **Guest OS ID** columns are the values Crucible's installer
automation actually supports today — they come straight from the platform's
install-mode list (`manual`, `cloudinit_cidata`, `debian_preseed`,
`windows_autounattend`). Anything not on this page hasn't been given a tested
recipe yet; pick the closest match and expect to do more by hand.

> [!note]
> **What's live in your lab right now** is a separate question from "what can be
> built." To see the templates students can currently deploy, go to
> **Admin → Templates** and look for ones in the **active** state. This page is
> about *building new* templates, not listing existing ones.

---

## How to read a recipe

Each recipe has:

1. **A settings table** — every value you type into the wizard, top to bottom.
   Anything not listed, **leave blank**; the wizard fills in a sensible default.
2. **Numbered steps** — the click-by-click, including anything you must do
   inside the Build Console.
3. **Gotchas** — the one or two things that bite people for this OS.
4. **You're done when…** — what success looks like before you Publish.

The **RAM / disk / vCPU** numbers are safe starting points. Students can grow
CPU and RAM later within their quota; **disk cannot shrink**, so don't
over-allocate.

---

## Linux recipes

### 1. Ubuntu Server 24.04 LTS — the easy path

> [!tip]
> This is the recommended Linux build. `cloudinit_cidata` installs itself
> hands-off **and** satisfies all five points of the
> [Linux template contract](templates.md#linux-template-contract) for you:
> open-vm-tools, cloud-init with the VMware datasource, the `student` default
> user, SSH host-key regeneration, the apt proxy, and passwordless sudo. You
> mostly wait.

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Linux` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Ubuntu **Server** 24.04 LTS ISO (e.g. `ubuntu-24.04-live-server-amd64.iso`) |
| **Install mode** | `cloudinit_cidata` |
| **Guest OS ID** | `ubuntu64Guest` |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `40` |
| **Firmware / vTPM** | EFI (the wizard's default). No vTPM needed. |
| **Unattended → Hostname** | `ubuntu-lab` (or leave blank) |
| **Unattended → Username** | `student` |
| **Unattended → Password** | `Changeme123!` |
| **Generalize method** | cloud-init clean (automatic — just click **Generalize**) |

**Steps**

1. Fill the form per the table and click **Create draft**.
2. On the template page, click **Provision**. The Ubuntu autoinstall runs
   hands-off (20–45 min). You do **not** need to open the console.
3. Wait for the state to reach `configuring`. (Optional: open the Build Console
   to install extra tools, then close it.)
4. Click **Generalize**, then — once state is `ready` — **Publish to students**.

**Gotchas**

- If the console ever shows `Continue with autoinstall? (yes|no)` stuck for more
  than a couple minutes, the auto-confirm failed — type `yes` to unblock and
  report it (see the autoinstall warning in
  [Provisioning from an ISO](templates.md#provisioning-from-an-iso)).
- The username **must** be `student`. Don't change it.

**You're done when…** the template reaches `active` after Publish. If you want
proof before publishing, open the console and run the
[verify-before-publishing block](templates.md#verify-before-publishing) — every
line should look right without you touching anything.

---

### 2. Ubuntu Desktop 24.04 LTS — you click through it

> [!warning]
> Ubuntu **Desktop** does **not** use the Server autoinstaller, so
> `cloudinit_cidata` will not drive it. You install it by hand
> (`Install mode = manual`) and then you must satisfy the
> [Linux template contract](templates.md#linux-template-contract) yourself —
> Desktop does not do it for you.

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Linux` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Ubuntu **Desktop** 24.04 LTS ISO (e.g. `ubuntu-24.04-desktop-amd64.iso`) |
| **Install mode** | `manual` |
| **Guest OS ID** | `ubuntu64Guest` |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `40` |
| **Firmware / vTPM** | EFI (default). No vTPM needed. |
| **Default username** (bottom of form) | `student` |
| **Default password** (bottom of form) | `Changeme123!` |
| **Generalize method** | cloud-init clean — **only after** you install cloud-init (step 4) |

**Steps**

1. Fill the form per the table and click **Create draft**.
2. Click **Provision**. When state reaches `configuring`, click
   **Open Build Console ↗**.
3. In the console, click through the Ubuntu Desktop installer. When it asks for
   your account, create the user **`student`** with password **`Changeme123!`**.
   Finish the install and let it reboot to the desktop.
4. Still in the console, open a Terminal and make the image satisfy the Linux
   contract. At minimum:
   ```bash
   sudo apt-get update
   sudo apt-get install -y open-vm-tools open-vm-tools-desktop cloud-init
   ```
   Then follow **every** step in
   [Linux template contract](templates.md#linux-template-contract): write
   `/etc/cloud/cloud.cfg.d/99-crucible.cfg` with `default_user.name: student`,
   install the `crucible-regen-ssh-hostkeys.service` unit, set the apt proxy,
   and add the passwordless-sudo drop-in. Finish with the
   [verify block](templates.md#verify-before-publishing) — especially
   `sudo -n true && echo "sudo OK"`.
5. Close the console. Click **Generalize**, then **Publish to students**.

**Gotchas**

- Install `open-vm-tools-desktop` (not just `open-vm-tools`) so the console
  resizes and the clipboard works.
- The single most common silent failure is a default user that isn't `student`,
  or `sudo -n true` prompting for a password. Do not skip the verify block.

**You're done when…** the verify block passes inside the guest (every line OK,
`sudo OK` printed) and the template reaches `active` after Publish.

---

### 3. Debian 12 / 13 — hands-off via preseed

> [!note]
> `debian_preseed` **remasters the installer ISO** (it injects the preseed into
> the Debian installer itself, because debian-installer refuses to read a
> preseed from a second CD). That's normal and automatic — the build just takes
> a few extra minutes at the start. The preseed installs `open-vm-tools` and
> `cloud-init` and grants the `student` user passwordless sudo.

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Linux` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Debian 12 (or 13) netinst/DVD ISO (e.g. `debian-12.x.0-amd64-netinst.iso`) |
| **Install mode** | `debian_preseed` |
| **Guest OS ID** | `debian12_64Guest` (use the closest Debian entry the wizard offers for your version) |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `40` |
| **Firmware / vTPM** | EFI (default). No vTPM needed. |
| **Unattended → Hostname** | `debian-lab` (or blank) |
| **Unattended → Username** | `student` |
| **Unattended → Password** | `Changeme123!` |
| **Generalize method** | cloud-init clean (automatic — click **Generalize**) |

**Steps**

1. Fill the form per the table and click **Create draft**.
2. Click **Provision**. Debian installs hands-off (20–45 min). No console needed.
3. When state reaches `configuring`, optionally open the console to add tools,
   then close it.
4. Click **Generalize**, then **Publish to students**.

**Gotchas**

- The preseed installs `open-vm-tools` and `cloud-init` and sets passwordless
  sudo, but you should still confirm the
  [contract](templates.md#linux-template-contract) with the verify block —
  particularly that cloud-init's default user is `student` and
  `sudo -n true` succeeds. On Debian the installer creates the account, so the
  cloud-init `default_user` block does **not** re-grant sudo; the preseed's
  `/etc/sudoers.d/90-crucible-student` drop-in is what makes it work.
- If your Debian version's Guest OS ID isn't in the dropdown, pick the nearest
  Debian 64-bit entry; the install mode is what matters most.

**You're done when…** the template reaches `active` and the verify block passes.

---

### 4. Linux Mint 22.x (MATE) — read the tradeoff first

> [!danger]
> **Mint ships no cloud-init.** Crucible delivers each student's unique password
> through cloud-init's VMware datasource. On a stock Mint image that channel does
> not exist, so **every student clone would silently reject the password**. You
> must pick one of the two options below *before* you Generalize.

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Linux` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Linux Mint 22.x **MATE** ISO (e.g. `linuxmint-22-mate-64bit.iso`) |
| **Install mode** | `manual` |
| **Guest OS ID** | `ubuntu64Guest` (Mint 22 is built on Ubuntu 24.04) |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `40` |
| **Firmware / vTPM** | EFI (default). No vTPM needed. |
| **Generalize method** | Depends on the option you choose below |

**Option A — make Mint behave like Ubuntu (per-student passwords work)**

Choose this if you want each student to get their own random password.

1. Provision, open the console, and click through the Mint installer. Create the
   **`student`** user with **`Changeme123!`**.
2. In a Terminal, install cloud-init and open-vm-tools, then satisfy the **whole**
   [Linux template contract](templates.md#linux-template-contract):
   ```bash
   sudo apt-get update
   sudo apt-get install -y open-vm-tools open-vm-tools-desktop cloud-init
   ```
   Write `99-crucible.cfg` (`default_user.name: student`), install the host-key
   regen unit, set the apt proxy, add passwordless sudo, then run the
   [verify block](templates.md#verify-before-publishing).
3. Set **Default username** = `student`, **Default password** = `Changeme123!`
   on the template. Generalize (cloud-init clean) and Publish.

**Option B — static shared login (no per-student passwords)**

Choose this if you don't need unique passwords and want the simplest path.

1. Build Mint the same way, creating `student` / `Changeme123!`.
2. Mark the template as **not customizable** so Crucible doesn't try to inject a
   password it can't deliver. In practice that means the template is cloned
   without customization (`clone_no_customize`) and you set static
   **Default username** = `student`, **Default password** = `Changeme123!` on
   the template row so Crucible shows students **real, working** credentials.
   Every student then shares that login.
3. Generalize and Publish.

> [!warning]
> Do **not** ship a *customizable* Mint template with no cloud-init. That is the
> guaranteed silent lockout described in the
> [contract's Mint warning](templates.md#the-five-requirements): the pod looks
> healthy and green, but the student's password is refused. Pick Option A or B
> explicitly.

**You're done when…**
- **Option A:** the verify block passes and a test deploy logs in with the
  *per-pod* password shown on the pod page.
- **Option B:** a test deploy logs in with the *static* `student` /
  `Changeme123!` you set on the template.

---

## Windows recipes

All Windows recipes use `Install mode = windows_autounattend`. Crucible writes
an `autounattend.xml` seed disc that installs Windows unattended, creates the
`Student` account with a **sysprep-safe encoded password**, and installs
**cloudbase-init** so each student clone gets its own random password. Generalize
runs **sysprep /generalize** automatically. Give Windows more disk and RAM than
Linux.

> [!important] The blank Windows VM Crucible builds has **no vTPM and no Secure
> Boot** (it's a pvscsi + EFI shell). That's fine for Windows 10 and Server, but
> Windows 11 refuses to install without them — see recipe 6 and the engineering
> note `docs/architecture/iso-build-hardware-gaps.md` for the supported
> workaround.

### 5. Windows 10 Pro

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Windows` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Windows 10 Pro x64 ISO (e.g. `Win10_22H2_x64.iso`) |
| **Install mode** | `windows_autounattend` |
| **Guest OS ID** | `windows9_64Guest` |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `60` |
| **Firmware / vTPM** | EFI (default). **No vTPM required.** |
| **Unattended → Hostname** | `win10-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `Changeme123!` |
| **Generalize method** | sysprep /generalize (automatic — click **Generalize**) |

**Steps**

1. Fill the form per the table and click **Create draft**.
2. Click **Provision**. Windows Setup runs unattended (30–60 min).
3. When state reaches `configuring`, optionally open the console to install
   software, then close it.
4. Click **Generalize** (runs sysprep), then **Publish to students**.

**Gotchas**

- Don't retype the password into a plaintext field anywhere — the wizard stores
  the sysprep-safe encoded form for you.
- If Setup stops at a disk-selection screen, see the pvscsi note under the
  Server recipes; Windows 10 media normally has the pvscsi driver, so this is
  rare here.

**You're done when…** the template reaches `active` and a test deploy logs in as
`Student` with the *per-pod* password from the pod page.

---

### 6. Windows 11 Pro — needs the TPM/Secure-Boot bypass

> [!danger]
> **This is the one Windows recipe with an extra required step.** The blank VM
> has no vTPM and no Secure Boot, but Windows 11 Setup hard-blocks on both. To
> install unattended on Crucible's shell you must add the documented
> **LabConfig registry bypass** to the answer file. Without it, Setup stops with
> *"This PC can't run Windows 11."*

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Windows` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Windows 11 Pro x64 ISO (e.g. `Win11_23H2_x64.iso`) |
| **Install mode** | `windows_autounattend` |
| **Guest OS ID** | `windows9_64Guest` |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `60` |
| **Firmware / vTPM** | EFI, **no vTPM** — this is exactly why the bypass is required |
| **Unattended → Hostname** | `win11-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `Changeme123!` |
| **Generalize method** | sysprep /generalize (automatic) |

**The required bypass**

Windows 11 Setup checks for TPM 2.0, Secure Boot, and a RAM floor during the
**windowsPE** phase, *before* it will touch the disk. The fix is four registry
values under `HKLM\SYSTEM\Setup\LabConfig`, plus one that lets the machine
finish OOBE with a **local** account (no Microsoft-account/network requirement):

| LabConfig value (DWORD `1`) | What it skips |
|-----------------------------|---------------|
| `BypassTPMCheck` | The TPM 2.0 requirement |
| `BypassSecureBootCheck` | The Secure Boot requirement |
| `BypassRAMCheck` | The 4 GB RAM floor |
| `BypassStorageCheck` | The 64 GB system-disk floor (harmless to include) |
| `BypassNRO` (under `...\Setup`, not LabConfig) | Forces the "no internet / local account" OOBE path |

As an `autounattend.xml` `windowsPE`-pass snippet (add a
`RunSynchronous`/`RunSynchronousCommand` block that runs these `reg add`
commands before setup partitions the disk):

```xml
<settings pass="windowsPE">
  <component name="Microsoft-Windows-Setup" processorArchitecture="amd64"
    publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS"
    xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
    <RunSynchronous>
      <RunSynchronousCommand wcm:action="add">
        <Order>1</Order>
        <Path>reg add HKLM\SYSTEM\Setup\LabConfig /v BypassTPMCheck /t REG_DWORD /d 1 /f</Path>
      </RunSynchronousCommand>
      <RunSynchronousCommand wcm:action="add">
        <Order>2</Order>
        <Path>reg add HKLM\SYSTEM\Setup\LabConfig /v BypassSecureBootCheck /t REG_DWORD /d 1 /f</Path>
      </RunSynchronousCommand>
      <RunSynchronousCommand wcm:action="add">
        <Order>3</Order>
        <Path>reg add HKLM\SYSTEM\Setup\LabConfig /v BypassRAMCheck /t REG_DWORD /d 1 /f</Path>
      </RunSynchronousCommand>
      <RunSynchronousCommand wcm:action="add">
        <Order>4</Order>
        <Path>reg add HKLM\SYSTEM\Setup\LabConfig /v BypassStorageCheck /t REG_DWORD /d 1 /f</Path>
      </RunSynchronousCommand>
      <RunSynchronousCommand wcm:action="add">
        <Order>5</Order>
        <Path>reg add HKLM\SYSTEM\Setup /v BypassNRO /t REG_DWORD /d 1 /f</Path>
      </RunSynchronousCommand>
    </RunSynchronous>
  </component>
</settings>
```

> [!note]
> **Where this snippet lives.** Crucible generates the Windows answer file for
> you today, and it does **not** yet include this `windowsPE` block (the
> generator only writes the `specialize` and `oobeSystem` passes). Until it does,
> a Windows 11 ISO build needs this block added to the generated
> `autounattend.xml`. This is tracked as **Gap A** in the engineering note
> `docs/architecture/iso-build-hardware-gaps.md` — read it before attempting a
> Win11 template, and coordinate with an admin so the seed disc carries the
> bypass. The alternative long-term fix (give the shell a real vTPM + Secure
> Boot) is **Gap A option (ii)** in that note.

**Steps**

1. Fill the form per the table and click **Create draft**.
2. Ensure the generated `autounattend.xml` includes the `windowsPE` LabConfig
   block above (see the note — this may need admin help until the generator adds
   it).
3. Click **Provision**. With the bypass in place, Setup installs unattended.
4. When state reaches `configuring`, optionally customize, then **Generalize**
   and **Publish**.

**You're done when…** Setup completes without the *"This PC can't run Windows 11"*
screen and the template reaches `active`.

---

### 7. Windows Server 2016

> [!warning]
> **Storage-driver caveat.** Older Server installers may not include the VMware
> **pvscsi** driver, so Setup shows *"We couldn't find any drives"* on Crucible's
> pvscsi shell. If that happens, the fix is to give this build an **LSI SAS**
> disk controller instead of pvscsi (LSI SAS has an in-box Windows driver). See
> **Gap B** in `docs/architecture/iso-build-hardware-gaps.md` — the blank-shell
> builder uses pvscsi by default, and changing the controller is an admin/code
> step, not a wizard field.

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Windows` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Windows Server 2016 x64 ISO (e.g. `WinServer2016_x64.iso`) |
| **Install mode** | `windows_autounattend` |
| **Guest OS ID** | `windows2019srv_64Guest` (closest supported Server entry) |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `60` |
| **Firmware / vTPM** | EFI (default). No vTPM required. |
| **Unattended → Hostname** | `ws2016-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `Changeme123!` |
| **Generalize method** | sysprep /generalize (automatic) |

**Steps** — same as Windows 10 (Provision → configure → Generalize → Publish),
but if Setup can't see the disk, stop and arrange the LSI SAS controller fix
from Gap B before retrying.

**You're done when…** Setup finds the disk, completes unattended, and the
template reaches `active`.

---

### 8. Windows Server 2019

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Windows` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Windows Server 2019 x64 ISO (e.g. `WinServer2019_x64.iso`) |
| **Install mode** | `windows_autounattend` |
| **Guest OS ID** | `windows2019srv_64Guest` |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `60` |
| **Firmware / vTPM** | EFI (default). No vTPM required. |
| **Unattended → Hostname** | `ws2019-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `Changeme123!` |
| **Generalize method** | sysprep /generalize (automatic) |

**Steps** — identical to Windows 10: Provision → (optional) configure →
Generalize → Publish. Server 2019 media generally sees the pvscsi disk; if not,
apply the Gap B controller fix.

**You're done when…** the template reaches `active` and a test deploy logs in as
`Student` with the per-pod password.

---

### 9. Windows Server 2025

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Windows` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Windows Server 2025 x64 ISO (e.g. `WinServer2025_x64.iso`) |
| **Install mode** | `windows_autounattend` |
| **Guest OS ID** | `windows2019srvNext_64Guest` |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `60` |
| **Firmware / vTPM** | EFI (default). No vTPM required. |
| **Unattended → Hostname** | `ws2025-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `Changeme123!` |
| **Generalize method** | sysprep /generalize (automatic) |

**Gotchas**

- **BitLocker cmdlets are missing by default.** Crucible's generalize step tries
  to auto-decrypt any BitLocker volume, but Server 2025 doesn't ship
  `manage-bde` / the BitLocker PowerShell module out of the box, so that step
  simply **no-ops** — which is fine for a lab image that never enabled
  BitLocker. Don't be alarmed if the generalize log mentions skipping it.
- **Large cumulative updates can fail over COM.** If you install Windows Updates
  in the console and a big cumulative update errors out, drive it from
  **Settings → Windows Update** (or `UsoClient StartInstall`) rather than a
  scripted COM call, which can fail on WS2025.
- Storage: if Setup can't see the disk, apply the Gap B controller fix like the
  other Server builds.

**You're done when…** the template reaches `active`; the generalize log
skipping BitLocker is expected, not an error.

---

## How a student deploys and logs in

Once your template is `active` and **public**, this is what a student does — and
where their password comes from:

1. The student clicks **Deploy VM** (or **New Environment**) in the Crucible UI.
2. They **pick your template** (or a blueprint that includes it) from the catalog.
3. Crucible clones a fresh copy onto the student's own isolated pod network and,
   for a customizable template, **generates a brand-new random password just for
   that student** and injects it into the clone (cloud-init on Linux,
   cloudbase-init on Windows).
4. That unique password is shown on the **pod page**. The student logs in as
   `student` (Linux) or `Student` (Windows) with the password from their pod
   page — **never** `Changeme123!`.

> [!important]
> `Changeme123!` only ever lives on the template you build. Each student gets a
> different password. If you chose Mint **Option B** (static login), that's the
> one exception: those students share the `student` / `Changeme123!` you set on
> the template row, because that template opts out of per-student customization.

---

## Where to go next

| I want to… | Go to |
|------------|-------|
| Understand the full wizard + Linux contract | [Building a Template](templates.md) |
| Know why Windows 11 needs a bypass / how to fix it properly | `docs/architecture/iso-build-hardware-gaps.md` |
| Look up a term | [Glossary](glossary.md) |
| See the big picture | [Overview](overview.md) |
