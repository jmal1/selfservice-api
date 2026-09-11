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
  operating system it's building. In the wizard this is the **Guest OS**
  dropdown: pick the entry whose name matches your OS and Crucible fills in the
  code for you. Each recipe below lists the exact code so you can confirm the
  right dropdown entry. If your OS isn't in the list, choose **Other
  (advanced)** at the bottom of the dropdown and type the code from the recipe
  by hand — any valid `…Guest` code is accepted, so uncommon or brand-new OSes
  still work.
- **Generalize** — the wizard button that cleans the finished VM so every
  student gets a unique copy. What "clean" means differs per OS; each recipe
  says which method applies.
- **The Linux template contract** — the five things a Linux image must have so
  each student's random password actually works. Full details:
  [Linux template contract](templates.md#linux-template-contract). Some recipes
  satisfy it for you; some make you do it by hand.

The standard build login is `Student` / `REPLACE_WITH_BUILD_PASSWORD`. Use it whenever a
recipe or the wizard asks you to make up a password. This is **not** the
password students get: Crucible generates a fresh random password per student.
See [The standard build login](templates.md#the-standard-build-login-studentchangeme123).

---

## Which templates exist / are supported today

Crucible can build a template from three kinds of source (the **Source type**
field in the wizard):

| Source type | When you'd use it | Covered on this page? |
|-------------|-------------------|-----------------------|
| **Clone an existing Crucible template** | Fastest, safest — start from a template that already works | No — see [templates.md](templates.md); no OS install needed |
| **Clone an existing vCenter VM** | Build from a VM an admin points you at | No — no OS install needed |
| **OVF / imported OVA** | Build from an OVA already imported via **Admin → Images** (`source_ref` = VM moref) | No — no OS install needed; see [templates.md](templates.md) |
| **ISO install** | Install an OS from scratch off an installer disc | **Yes — this whole page** |

The OSes below are the ones the ISO-install path is known to handle, plus the
Windows Server **L1 gold** clone path for 2019/2022. The **install mode** and
**Guest OS ID** columns are the values Crucible's installer automation actually
supports today — they come straight from the platform's install-mode list
(`manual`, `cloudinit_cidata`, `debian_preseed`, `windows_autounattend`).
**Windows Server 2025 is BLOCKED** (recipe 10) — do not use it as a build gate.
Anything else not on this page hasn't been given a tested recipe yet; pick the
closest match and expect to do more by hand.

**What is live in your lab** is separate from what can be built. To see the
templates students can deploy, go to **Admin → Templates** and look for ones in
the **active** state. This page is about building new templates.

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

This is the recommended Linux build. `cloudinit_cidata` installs hands-off and
satisfies the [Linux template contract](templates.md#linux-template-contract):
open-vm-tools, cloud-init with the VMware datasource, the `student` default
user, SSH host-key regeneration, the apt proxy, and passwordless sudo.

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Linux` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Ubuntu **Server** 24.04 LTS ISO — in this lab it is `ubuntu-24.04.3-live-server-amd64.iso` |
| **Install mode** | `cloudinit_cidata` |
| **Guest OS ID** | `ubuntu64Guest` |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `50` (40–60 is fine) |
| **Firmware / vTPM** | EFI (the wizard's default). No vTPM needed. |
| **Unattended → Hostname** | `ubuntu-lab` (or leave blank) |
| **Unattended → Username** | `student` |
| **Unattended → Password** | `REPLACE_WITH_BUILD_PASSWORD` |
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

### 2. Ubuntu Desktop 24.04 LTS — same easy path as Server

Ubuntu **Desktop** 23.04 and later (including 24.04) use the same **subiquity**
autoinstaller as Server, so `cloudinit_cidata` installs hands-off just as in
recipe 1. For a Desktop image, also install the **desktop** VMware tools for
console resizing and clipboard support.

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Linux` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Ubuntu **Desktop** 24.04 LTS ISO — in this lab it is `ubuntu-24.04.3-desktop-amd64.iso` |
| **Install mode** | `cloudinit_cidata` |
| **Guest OS ID** | `ubuntu64Guest` |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `50` (40–60 is fine) |
| **Firmware / vTPM** | EFI (default). No vTPM needed. |
| **Unattended → Hostname** | `ubuntu-desktop-lab` (or blank) |
| **Unattended → Username** | `student` |
| **Unattended → Password** | `REPLACE_WITH_BUILD_PASSWORD` |
| **Generalize method** | cloud-init clean (automatic — click **Generalize**) |

**Steps**

1. Fill the form per the table and click **Create draft**.
2. Click **Provision**. The autoinstall runs hands-off (20–45 min); the seed
   disc Crucible writes is volume-labelled **`CIDATA`** for you — you never build
   it by hand.
3. When state reaches `configuring`, click **Open Build Console ↗**, open a
   Terminal, and add the **desktop** tools package (the auto-install only put in
   the base `open-vm-tools`):
   ```bash
   sudo apt-get update && sudo apt-get install -y open-vm-tools-desktop
   ```
   Close the console when done.
4. Click **Generalize**, then — once state is `ready` — **Publish to students**.

**Gotchas**

- `cloudinit_cidata` already satisfies the whole
  [Linux template contract](templates.md#linux-template-contract) for you (the
  same generator drives Server and Desktop): default user `student`, cloud-init
  with the VMware datasource, host-key regen unit, passwordless sudo. You do
  **not** redo the contract by hand.
- **If per-clone passwords don't apply on a test deploy**, the usual Desktop-only
  cause is cloud-init's VMware customisation being disabled. Confirm the image's
  `/etc/cloud/cloud.cfg` does **not** contain `disable_vmware_customization: true`
  (it must be `false` or absent) — the datasource that delivers each student's
  password relies on it.
- Install `open-vm-tools-desktop`, not just `open-vm-tools`, for clipboard and
  console-resize.
- **If the hands-off install stalls** at a graphical prompt (rare, and only on
  Desktop), fall back to `Install mode = manual` and satisfy the contract by hand
  exactly like **Mint Option A** in recipe 4.

**You're done when…** the template reaches `active` after Publish and a test
deploy logs in with the *per-pod* password from the pod page. See the shared
[health-check checklist](#health-check-checklist-every-recipe).

---

### 3. Debian 12 / 13 — build it by hand today

> [!caution]
> **Read this before you pick an Install mode.** Debian's automated mode
> (`debian_preseed`) is **defined but not enabled in this build.** The
> debian-installer refuses to read a preseed from a second CD, so the seed would
> have to be baked into a *remastered* installer ISO — and Crucible cannot
> produce a bootable remastered ISO yet (it returns a "bootable preseed remaster
> is unsupported" error). If you select `debian_preseed`, **Provision stops with
> an error telling you to use `manual`.** So today you build Debian the same
> hands-on way as Ubuntu Desktop's fallback / Mint Option A.

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Linux` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Debian 12 (or 13) netinst/DVD ISO (e.g. `debian-12.x.0-amd64-netinst.iso`). **Not yet in this lab's ISO library — ask an admin to upload it first** (see *Missing ISOs* at the bottom). |
| **Install mode** | `manual` |
| **Guest OS ID** | `debian12_64Guest` for Debian 12 · `debian13_64Guest` for Debian 13 |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `40` (30–50 is fine) |
| **Firmware / vTPM** | EFI (default). No vTPM needed. |
| **Default username** (bottom of form) | `student` |
| **Default password** (bottom of form) | `REPLACE_WITH_BUILD_PASSWORD` |
| **Generalize method** | cloud-init clean — **only after** you install cloud-init (step 3) |

**Steps**

1. Fill the form per the table and click **Create draft**, then **Provision**.
   When state reaches `configuring`, click **Open Build Console ↗**.
2. Click through the Debian installer. Choose **not** to set a root password
   (leave it blank so `sudo` is used), and create the user **`student`** with
   **`REPLACE_WITH_BUILD_PASSWORD`**. Let it finish and reboot.
3. Open a Terminal and make the image satisfy the
   [Linux template contract](templates.md#linux-template-contract):
   ```bash
   su -                                  # or use sudo if you enabled it
   apt-get update
   apt-get install -y open-vm-tools cloud-init sudo
   ```
   Then, still as root, do the four contract pieces the Ubuntu autoinstall would
   have done for you:
   - Enable the VMware datasource so per-clone passwords arrive — write
     `/etc/cloud/cloud.cfg.d/99-crucible.cfg` with
     `datasource_list: [ VMware, NoCloud, None ]` and a `default_user` named
     `student` (copy the block from
     [the contract](templates.md#linux-template-contract)).
   - Grant `student` passwordless sudo:
     `echo 'student ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/90-crucible-student && chmod 440 /etc/sudoers.d/90-crucible-student`.
   - Install the SSH host-key regen unit from the contract.
   - Run `cloud-init clean` so the first clone re-applies its password.
   Finish with the [verify block](templates.md#verify-before-publishing) —
   especially `sudo -n true && echo "sudo OK"`.
4. Close the console. Click **Generalize**, then **Publish to students**.

**Gotchas**

- The most common silent failure is a default user that isn't `student`, or
  `sudo -n true` prompting for a password. Do not skip the verify block.
- **When the remaster feature lands**, this recipe switches to
  `Install mode = debian_preseed` with the same Guest OS IDs and the preseed will
  install `open-vm-tools`, `cloud-init` and passwordless sudo for you — but until
  Provision accepts it without erroring, use `manual`.

**You're done when…** the verify block passes inside the guest and the template
reaches `active`. See the shared
[health-check checklist](#health-check-checklist-every-recipe).

---

### 4. Linux Mint 22.x (MATE) — read the tradeoff first

> [!caution]
> **Mint ships no cloud-init.** Crucible delivers each student's unique password
> through cloud-init's VMware datasource. On a stock Mint image that channel does
> not exist, so **every student clone would silently reject the password**. You
> must pick one of the two options below *before* you Generalize.

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Linux` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Linux Mint 22.x **MATE** ISO — in this lab it is `linuxmint-22.3-mate-64bit.iso` |
| **Install mode** | `manual` |
| **Guest OS ID** | `ubuntu64Guest` (Mint 22 is built on Ubuntu 24.04) |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `50` (40–60 is fine) |
| **Firmware / vTPM** | EFI (default). No vTPM needed. |
| **Generalize method** | Depends on the option you choose below |

**Option A — make Mint behave like Ubuntu (per-student passwords work)**

Choose this if you want each student to get their own random password.

1. Provision, open the console, and click through the Mint installer. Create the
   **`student`** user with **`REPLACE_WITH_BUILD_PASSWORD`**.
2. In a Terminal, install cloud-init and open-vm-tools, then satisfy the **whole**
   [Linux template contract](templates.md#linux-template-contract):
   ```bash
   sudo apt-get update
   sudo apt-get install -y open-vm-tools open-vm-tools-desktop cloud-init
   ```
   Write `99-crucible.cfg` (`default_user.name: student`), install the host-key
   regen unit, set the apt proxy, add passwordless sudo, then run the
   [verify block](templates.md#verify-before-publishing).
3. Set **Default username** = `student`, **Default password** = `REPLACE_WITH_BUILD_PASSWORD`
   on the template. Generalize (cloud-init clean) and Publish.

**Option B — static shared login (no per-student passwords)**

Choose this if you don't need unique passwords and want the simplest path.

1. Build Mint the same way, creating `student` / `REPLACE_WITH_BUILD_PASSWORD`.
2. Mark the template as **not customizable** so Crucible doesn't try to inject a
   password it can't deliver. In practice that means the template is cloned
   without customization (`clone_no_customize`) and you set static
   **Default username** = `student`, **Default password** = `REPLACE_WITH_BUILD_PASSWORD` on
   the template row so Crucible shows students **real, working** credentials.
   Every student then shares that login.
3. Generalize and Publish.

**You're done when…**
- **Option A:** the verify block passes and a test deploy logs in with the
  *per-pod* password shown on the pod page.
- **Option B:** a test deploy logs in with the *static* `student` /
  `REPLACE_WITH_BUILD_PASSWORD` you set on the template.

---

## Windows recipes

All Windows recipes use `Install mode = windows_autounattend` **when you install
from ISO**. Crucible writes an `autounattend.xml` seed disc that installs
Windows unattended, creates the `Student` account with a **sysprep-safe encoded
password**, and installs **cloudbase-init** so each student clone gets its own
random password. Generalize runs **sysprep /generalize** automatically. Give
Windows more disk and RAM than Linux.

> [!important]
> **Windows Server SKU status (Wave A).**
>
> - **Server 2022 is gold (July 2026).** Prefer cloning the published L1
>   `student-windows-server-2022` (`clone_with_customize`, local account
>   `Student`, existing Cloudbase-Init guestinfo password). Operator L1 runbook:
>   `templates/windows/SERVER-2022-SETUP.md`.
> - **Server 2019 is the additional Server SKU this wave.** Same L1 pipeline as
>   2022 (answer ISO → WU → Cloudbase-Init → `sysprep /generalize /oobe /shutdown`
>   → wizard `base-image` at L2). Operator L1 runbook:
>   `templates/windows/SERVER-2019-SETUP.md`. A human still has to place
>   `WindowsServer2019.iso` and build that L1 on lab.
> - **Server 2025 is BLOCKED.** Microsoft `explorer.exe` crash `0xc0000409` on
>   first clone boot after sysprep, plus a Cloudbase-Init password failure.
>   Tracked as a known issue on the Homelab vault **[[Roadmap]]** (do not invent
>   a vault URL). Do **not** rebuild 2025 as a gate. Use 2022 (gold) or 2019.
>
> Do **not** use the deprecated Homelab vault
> `future/scripts/build-student-windows-server-*.ps1` scripts. Follow
> `templates/windows/` + this page. No new credential protocol — per-clone
> passwords stay on existing `clone_with_customize` / Cloudbase-Init.

**The per-clone password just works — do not set it by hand.** The answer file
stores the build password in Windows' encoded form
(`base64(UTF-16LE(cleartext + "Password"))`) in the `oobeSystem` pass, which
survives sysprep, and cloudbase-init re-applies a **fresh random password per
student** on first clone boot. Do not type a password into a plaintext field or
edit it in the XML; the pipeline substitutes the per-clone value.

The blank Windows VM Crucible builds has **no vTPM and no Secure Boot** (it is a
pvscsi + EFI shell). That is fine for Windows 10 and Server, but Windows 11
needs the supported workaround in recipe 6.

<a id="windows-storage-driver-pvscsi"></a>
> [!warning]
> **"We couldn't find any drives" affects EVERY Windows installer.** The blank
> shell uses a VMware **pvscsi** disk controller, and Windows Setup — for *all*
> of Windows 10, Windows 11, and Server 2016/2019/2022/2025 — has **no in-box
> pvscsi driver** (VMware KB 1010398). So any Windows ISO build can stop at the
> disk-selection screen showing no disks. Two ways to solve it:
>
> - **Ideal — inject the pvscsi driver during Setup's `windowsPE` pass.** This
>   needs the VMware Tools `pvscsi` driver on media Setup can read, referenced
>   from a `Microsoft-Windows-PnpCustomizationsWinPE` component
>   (`<DriverPaths><PathAndCredentials>`). The correct driver folder on the Tools
>   ISO is:
>   - **Win11 / Server 2022 / Server 2025** (Tools ≥ 12): `…\pvscsi\Win10\amd64`
>   - **Win10 / Server 2016 / Server 2019** (Tools ≥ 11.2): `…\pvscsi\Win8\amd64`
> - **Manual fallback at the screen** — click **Load driver**, browse to the
>   VMware Tools CD, select the `pvscsi` folder, click **Next**; the disk appears.
>
> ⚠️ **Constraint you must know today:** Crucible's blank shell mounts only **two**
> IDE CD-ROMs — the installer ISO and the autounattend **seed** ISO — and the
> seed ISO the pipeline builds carries *only* `autounattend.xml` (no driver
> files). So there is currently **no mounted VMware Tools CD** for either the
> `windowsPE` injection or the **Load driver** fallback to point at.
>
> **Windows Server builds are handled automatically (as of #122).** Crucible now
> builds **Server** shells on an **LSI SAS** controller (which *does* have an
> in-box Windows driver), so Server Setup sees the disk with no action from you —
> no Load-driver step, no admin involvement. **Client** Windows 10 / 11 still
> build on pvscsi, so they can still hit "no drives"; use the manual **Load
> driver** fallback above for those until the pipeline stages the pvscsi driver
> (Option 1) — see **Gap B** in
> `docs/architecture/iso-build-hardware-gaps.md`.

<a id="fully-patch-before-sysprep"></a>
> [!warning]
> **Windows 11 24H2 (build 26100): fully patch BEFORE you Generalize.**
> Un-patched build-26100 images hit a Microsoft shell bug where
> `explorer.exe` crashes with `0xc0000409` on the **first clone boot after
> sysprep /generalize**, leaving a gray screen. This is a Microsoft bug, **not** a
> Crucible defect. Fix: in the Build Console, install the **November 2024 (or
> later) cumulative update** so the image is build **≥ 26100.2314**, *then*
> Generalize. Win11 23H2 (build 22631) is unaffected. (Confirm the exact KB on
> the Windows Update history page for your build.)
>
> **Windows Server 2025 is BLOCKED for the same Microsoft `0xc0000409` explorer
> crash plus a Cloudbase-Init password failure.** Do not treat "patch then
> generalize" as a Server 2025 gate, and do not rebuild 2025 to unblock a
> course. Tracked on the Homelab vault **[[Roadmap]]**. Use Server 2022 (gold,
> July 2026) or Server 2019 instead — see [recipe 10](#10-windows-server-2025).

### 5. Windows 10 Pro

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Windows` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Windows 10 Pro x64 ISO — in this lab it is `Windows10.iso` |
| **Install mode** | `windows_autounattend` |
| **Guest OS ID** | `windows9_64Guest` |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `70` (60–80 is fine) |
| **Firmware / vTPM** | EFI (default). **No vTPM required.** |
| **Unattended → Hostname** | `win10-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `REPLACE_WITH_BUILD_PASSWORD` |
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
- Windows Setup has **no built-in pvscsi driver**, so it may stop at a
  disk-selection screen showing no drives. If it does, either (a) click
  **Load driver**, browse the mounted VMware Tools CD to the `pvscsi` folder and
  select it, or (b) have an admin switch the VM's disk controller to **LSI Logic
  SAS** (in-box driver). Full detail:
  [the Windows storage-driver note](#windows-storage-driver-pvscsi) and Gap B.

**You're done when…** the template reaches `active` and a test deploy logs in as
`Student` with the *per-pod* password from the pod page. See the shared
[health-check checklist](#health-check-checklist-every-recipe).

---

### 6. Windows 11 Pro — needs the TPM/Secure-Boot bypass

> [!caution]
> **This is the one Windows recipe with an extra required step.** The blank VM
> has no vTPM and no Secure Boot, but Windows 11 Setup hard-blocks on both. To
> install unattended on Crucible's shell you must add the documented
> **LabConfig registry bypass** to the answer file. Without it, Setup stops with
> *"This PC can't run Windows 11."*

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Windows` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Windows 11 Pro x64 ISO — in this lab it is `Win11_25H2_English_x64.iso` |
| **Install mode** | `windows_autounattend` |
| **Guest OS ID** | `windows11_64Guest` |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `90` (80–100; Win11 wants ≥ 64 GB) |
| **Firmware / vTPM** | EFI, **no vTPM** — this is exactly why the bypass is required |
| **Unattended → Hostname** | `win11-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `REPLACE_WITH_BUILD_PASSWORD` |
| **Generalize method** | sysprep /generalize (automatic) |

**The required bypass**

Windows 11 Setup checks for TPM 2.0, Secure Boot, a RAM floor, a CPU allow-list
and a system-disk floor during the **windowsPE** phase, *before* it will touch
the disk. The fix is five registry values under `HKLM\SYSTEM\Setup\LabConfig`,
plus one that lets the machine finish OOBE with a **local** account (no
Microsoft-account/network requirement):

| LabConfig value (DWORD `1`) | What it skips |
|-----------------------------|---------------|
| `BypassTPMCheck` | The TPM 2.0 requirement |
| `BypassSecureBootCheck` | The Secure Boot requirement |
| `BypassRAMCheck` | The 4 GB RAM floor |
| `BypassStorageCheck` | The 64 GB system-disk floor |
| `BypassCPUCheck` | The supported-CPU allow-list |
| `BypassNRO` (under `...\Setup`, not LabConfig) | Forces the "no internet / local account" OOBE path |

These five `Bypass*Check` keys are the authoritative set; they work unchanged on
**23H2 and 24H2**. `BypassNRO` is an extra convenience for the local-account OOBE
path. As an `autounattend.xml` `windowsPE`-pass snippet (add a
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
        <Path>reg add HKLM\SYSTEM\Setup\LabConfig /v BypassCPUCheck /t REG_DWORD /d 1 /f</Path>
      </RunSynchronousCommand>
      <RunSynchronousCommand wcm:action="add">
        <Order>6</Order>
        <Path>reg add HKLM\SYSTEM\Setup /v BypassNRO /t REG_DWORD /d 1 /f</Path>
      </RunSynchronousCommand>
    </RunSynchronous>
  </component>
</settings>
```

**The snippet is now automatic.** As of **PR #121**, Crucible's Windows
answer-file generator emits this `windowsPE` LabConfig block whenever the
template's Guest OS ID is `windows11_64Guest`, so you no longer add it by hand.
Win10 (`windows9_64Guest`) and every Windows Server answer file stay
byte-identical. The generated `autounattend.xml` still sits at the **root** of
the removable/seed volume. On an **older API image that predates #121**, add the
block to the generated `autounattend.xml` by hand and coordinate with an admin
so the seed disc carries it.

**Steps**

1. Fill the form per the table and click **Create draft**.
2. On a current api image the generator already includes the `windowsPE`
   LabConfig block for `windows11_64Guest` (see the note), so no manual step is
   needed; on an older image predating #121, add the block by hand
   (admin-assisted).
3. Click **Provision**. With the bypass in place, Setup installs unattended. If
   Setup shows **no disks**, that's the pvscsi issue — either (a) click
   **Load driver** and browse the VMware Tools CD to the `pvscsi` folder, or
   (b) have an admin switch the disk controller to **LSI Logic SAS**; see
   [the Windows storage-driver note](#windows-storage-driver-pvscsi).
4. When state reaches `configuring`, **fully patch the image first** if this is a
   24H2 (build 26100) media: install the Nov 2024+ cumulative update to reach
   build ≥ 26100.2314 — see
   [fully patch before sysprep](#fully-patch-before-sysprep). Then **Generalize**
   and **Publish**.

**You're done when…** Setup completes without the *"This PC can't run Windows 11"*
screen, the template reaches `active`, and a test clone boots to a desktop (no
gray screen) and logs in with its per-pod password. See the shared
[health-check checklist](#health-check-checklist-every-recipe).

---

> [!warning]
> **⚠️ Guest OS ID naming trap (Windows Server).** The vSphere API's Server IDs
> do **not** line up with the year on the box — the `-Next` IDs are shifted one
> release forward, and WS2016 has no `2016` id at all. Copy these exactly:
>
> | Windows Server version | Guest OS ID |
> |------------------------|-------------|
> | Server 2016 | `windows9Server64Guest` (shares the Win10 kernel) |
> | Server 2019 | `windows2019srv_64Guest` |
> | Server 2022 | `windows2019srvNext_64Guest` ("2019**Next**") |
> | Server 2025 | `windows2022srvNext_64Guest` ("2022**Next**") |
>
> Future editors: don't "correct" these to match the year — the mismatch is
> intentional and verified against the govmomi `vim25/types/enum.go` enum.

### 7. Windows Server 2016

**Storage driver is handled automatically (as of #122).** Server 2016 Setup has
no in-box pvscsi driver, so Crucible builds **Server** shells on an **LSI SAS**
controller with an in-box Windows driver. `windows9Server64Guest` Setup sees the
disk with no Load-driver step or wizard field.

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Windows` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Windows Server 2016 x64 ISO (e.g. `WinServer2016_x64.iso`). **Not yet in this lab's ISO library — ask an admin to upload it first** (see *Missing ISOs* at the bottom). |
| **Install mode** | `windows_autounattend` |
| **Guest OS ID** | `windows9Server64Guest` (Windows Server 2016; shares the Windows 10 kernel, so the ID says "windows9Server") |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `70` (60–80 is fine) |
| **Firmware / vTPM** | EFI (default). No vTPM required. |
| **Unattended → Hostname** | `ws2016-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `REPLACE_WITH_BUILD_PASSWORD` |
| **Generalize method** | sysprep /generalize (automatic) |

**Steps** — same as Windows 10 (Provision → configure → Generalize → Publish).
Setup sees the disk automatically because Server shells build on LSI SAS (Gap B,
Option 2, #122) — no Load-driver step needed.

If your vCenter's **Guest OS ID** dropdown does not list
`windows9Server64Guest`, use `windows2019srv_64Guest` (Server 2019's ID) as the
nearest alternative. The install still works; only optimization hints differ.

**You're done when…** Setup finds the disk, completes unattended, and the
template reaches `active`. See the shared
[health-check checklist](#health-check-checklist-every-recipe).

---

### 8. Windows Server 2019

**Preferred: clone the L1 gold VM** after an admin builds it. That L1 matches
the Server 2022 (July 2026) pipeline: answer ISO → Windows Update →
Cloudbase-Init (`username=Student`) → `sysprep /generalize /oobe /shutdown` →
leave powered off (no L1 snapshot) → register for `clone_with_customize` →
the wizard takes `base-image` at L2. Operator runbook (plain path, not a wiki
page): `templates/windows/SERVER-2019-SETUP.md`. Do **not** use deprecated
vault `future/scripts/build-student-windows-server-*.ps1`.

Until that L1 exists on lab, a human must place **`WindowsServer2019.iso`** at
`[NAS-BackupsAndISOS] ISOs/Windows/WindowsServer2019.iso` (see *Missing ISOs*).

#### L1 deltas vs Server 2022 gold

| Topic | Server 2022 (gold) | Server 2019 |
|-------|--------------------|-------------|
| guestId | `windows2019srvNext_64Guest` | **`windows2019srv_64Guest`** (no `Next`) |
| Install ISO | `WindowsServer2022.iso` (already on NAS) | **`WindowsServer2019.iso`** (place first) |
| Answer ISO / XML | `ws2022-autounattend.iso` / `autounattend-server2022.xml` | **`ws2019-autounattend.iso` / `autounattend-server2019.xml`** |
| WIM `/IMAGE/NAME` | `Windows Server 2022 SERVERSTANDARD` | **`Windows Server 2019 SERVERSTANDARD`** (not `…CORE`) |
| Disk controller | LSI Logic SAS | **LSI Logic SAS** (same — WS2019 WinPE has no reliable in-box pvscsi) |
| ComputerName (build) | `WIN-WS2022-L1` | `WIN-WS2019-L1` |
| pvscsi inject folder | Tools `pvscsi\Win10\amd64` | Tools **`pvscsi\Win8\amd64`** |
| Local account / passwords | `Student` + existing `clone_with_customize` / Cloudbase-Init | **Identical** |
| Sysprep | `/generalize /oobe /shutdown` once | **Identical** |

#### After L1 is published — clone it (instructors)

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Windows` |
| **Source type** | **Clone an existing Crucible template** (or **Clone an existing vCenter VM** if the L1 is not yet a Crucible row) |
| **Source** | `Windows Server 2019` / vCenter VM `student-windows-server-2019` |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `REPLACE_WITH_BUILD_PASSWORD` (bootstrap only — clones get a random password) |
| **Generalize method** | sysprep /generalize (automatic — creates `base-image`) |

kind is `clone_with_customize`. Per-clone passwords stay on Cloudbase-Init
guestinfo — no new credential protocol.

#### From-scratch ISO install (only if L1 is not published)

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Windows` |
| **Source type** | `ISO install` |
| **ISO** | Pick `WindowsServer2019.iso` after an admin uploads it (see *Missing ISOs*). |
| **Install mode** | `windows_autounattend` |
| **Guest OS ID** | `windows2019srv_64Guest` |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `70` (60–80 is fine) |
| **Firmware / vTPM** | EFI (default). No vTPM required. |
| **Unattended → Hostname** | `ws2019-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `REPLACE_WITH_BUILD_PASSWORD` |
| **Generalize method** | sysprep /generalize (automatic) |

**Steps** — identical to Windows 10: Provision → (optional) configure →
Generalize → Publish. Setup sees the disk automatically — Server shells build on
LSI SAS (Gap B, Option 2, #122), so no Load-driver step is needed. If you ever
inject pvscsi by hand, use Tools `pvscsi\Win8\amd64`.

**You're done when…** the template reaches `active` and a test deploy logs in as
`Student` with the per-pod password. See the shared
[health-check checklist](#health-check-checklist-every-recipe).

---

### 9. Windows Server 2022

**Gold L1 (July 2026).** Prefer cloning the published
`student-windows-server-2022` template (`clone_with_customize`, local account
`Student`). Operator L1 runbook: `templates/windows/SERVER-2022-SETUP.md`.
ISO-install from `WindowsServer2022.iso` remains available if you need a
from-scratch rebuild.

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Windows` |
| **Source type** | **Clone an existing Crucible template** (preferred) or `ISO install` |
| **ISO** | Only for from-scratch: `WindowsServer2022.iso` |
| **Install mode** | `windows_autounattend` (ISO path only) |
| **Guest OS ID** | `windows2019srvNext_64Guest` ⚠️ ("2019**Next**" = Server 2022, **not** 2019) |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `70` (60–80 is fine) |
| **Firmware / vTPM** | EFI (default). No vTPM required for the base install. |
| **Unattended → Hostname** | `ws2022-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `REPLACE_WITH_BUILD_PASSWORD` |
| **Generalize method** | sysprep /generalize (automatic) |

**Steps** — clone the gold L1 (or ISO-install like Windows 10): Provision →
(optional) configure → Generalize → Publish. Server shells build on LSI SAS
(Gap B, Option 2, #122), so no Load-driver step is needed.

**You're done when…** the template reaches `active` and a test deploy logs in as
`Student` with the per-pod password. See the shared
[health-check checklist](#health-check-checklist-every-recipe).

---

<a id="10-windows-server-2025"></a>
### 10. Windows Server 2025

> [!danger]
> **BLOCKED — do not build or rebuild Server 2025 as a gate.**
>
> First clone boot after `sysprep /generalize` hits Microsoft `explorer.exe`
> `0xc0000409` (gray desktop), and Cloudbase-Init fails to apply the per-clone
> password. This is tracked as a known issue on the Homelab vault
> **[[Roadmap]]** (do not invent a vault URL). Patching cumulative updates is
> **not** an accepted unblock for this SKU in Crucible.
>
> Use **Server 2022 (gold, July 2026)** or **Server 2019** instead.

Historical guest-OS ID only (so nobody "corrects" the enum table above):
`windows2022srvNext_64Guest` ("2022**Next**" = Server 2025). The ISO
`WindowsServer2025.iso` may still appear in the picker; **do not** start a
new template from it. Operator notes retained for history only:
`templates/windows/SERVER-2025-SETUP.md`.

**You're done when…** you did **not** publish a Server 2025 template. Pick
recipe 8 or 9.

---

## Health-check checklist (every recipe)

Before you click **Publish to students**, confirm all seven. Every "You're done
when…" above points here.

1. **The VM boots** to a login screen (Linux) or desktop (Windows) with no error
   dialog.
2. **VMware Tools is running** — Linux: `systemctl is-active open-vm-tools` says
   `active`; Windows: **VMware Tools** shows Running in Services, and the
   template page shows Tools detected.
3. **The default user is right** — Linux `student`, Windows `Student`.
4. **Passwordless sudo works** (Linux) — `sudo -n true && echo "sudo OK"` prints
   `sudo OK` with no password prompt. (Skip on Windows.)
5. **Generalize succeeded** — the template state reached `ready`, and (Windows)
   the sysprep log shows no fatal error; (Linux) `cloud-init clean` ran.
6. **A test deploy gets a per-clone password** — deploy the template once and
   confirm the pod page shows a **random** password (not `REPLACE_WITH_BUILD_PASSWORD`).
7. **You can log in with that per-pod password** as `student` / `Student`.

If any of these fail, do **not** Publish — fix it and re-verify. For the copy-me
in-guest verification commands, use the
[verify-before-publishing block](templates.md#verify-before-publishing).

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
   page — **never** `REPLACE_WITH_BUILD_PASSWORD`.

`REPLACE_WITH_BUILD_PASSWORD` only lives on the template you build. Each student gets a
different password, except with Mint **Option B** (static login): those students
share the `student` / `REPLACE_WITH_BUILD_PASSWORD` set on the template row because that
template opts out of per-student customization.

---

## Missing ISOs (upload these before using those recipes)

Three recipes above point here because their installer ISO is **not yet in this
lab's ISO library**, so the wizard's **ISO source** dropdown won't list them until
an admin uploads them to the `NAS-BackupsAndISOS` datastore (`ISOs/…`):

| OS | Recipe | Filename to place | Get the ISO from |
|----|--------|-------------------|------------------|
| Debian 12 / 13 | Recipe 3 (Debian) | as published on debian.org | debian.org → the amd64 netinst/DVD image |
| Windows Server 2016 | Server recipes | e.g. `WinServer2016_x64.iso` | Microsoft Evaluation Center / VLSC |
| Windows Server 2019 | Recipe 8 + L1 runbook | **`WindowsServer2019.iso`** at `ISOs/Windows/WindowsServer2019.iso` | Microsoft Evaluation Center / VLSC (Standard Desktop Experience Eval) |

The Server 2019 L1 also needs the answer ISO `ws2019-autounattend.iso` built
from `templates/windows/autounattend-server2019.xml` — see
`templates/windows/SERVER-2019-SETUP.md`.

Everything else in this guide (Ubuntu Server/Desktop 24.04.3, Linux Mint 22.3,
Windows 10, Windows 11 25H2, Windows Server 2022) **is already uploaded** and
will appear in the picker with the exact filename shown in each recipe's **ISO**
row. `WindowsServer2025.iso` may also be on the datastore; that SKU is
[blocked](#10-windows-server-2025) — do not start a new template from it.

---

## Where to go next

| I want to… | Go to |
|------------|-------|
| Understand the full wizard + Linux contract | [Building a Template](templates.md) |
| Know why Windows 11 needs a bypass / how to fix it properly | `docs/architecture/iso-build-hardware-gaps.md` |
| Look up a term | [Glossary](glossary.md) |
| See the big picture | [Overview](overview.md) |
