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

The ten OSes below are the ones the ISO-install path is known to handle. The
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
| **Disk (GB)** | `50` (40–60 is fine) |
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

### 2. Ubuntu Desktop 24.04 LTS — same easy path as Server

> [!tip]
> Ubuntu **Desktop** 23.04 and later (so 24.04 too) ship the same **subiquity**
> autoinstaller as Server, so it uses `cloudinit_cidata` and installs hands-off
> just like recipe 1. The only extra thing you want on a Desktop image is the
> **desktop** flavour of the VMware tools so the console resizes and the
> clipboard works.

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Linux` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Ubuntu **Desktop** 24.04 LTS ISO (e.g. `ubuntu-24.04-desktop-amd64.iso`) |
| **Install mode** | `cloudinit_cidata` |
| **Guest OS ID** | `ubuntu64Guest` |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `50` (40–60 is fine) |
| **Firmware / vTPM** | EFI (default). No vTPM needed. |
| **Unattended → Hostname** | `ubuntu-desktop-lab` (or blank) |
| **Unattended → Username** | `student` |
| **Unattended → Password** | `Changeme123!` |
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

> [!danger]
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
| **ISO** | Pick the Debian 12 (or 13) netinst/DVD ISO (e.g. `debian-12.x.0-amd64-netinst.iso`) |
| **Install mode** | `manual` |
| **Guest OS ID** | `debian12_64Guest` for Debian 12 · `debian13_64Guest` for Debian 13 |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `40` (30–50 is fine) |
| **Firmware / vTPM** | EFI (default). No vTPM needed. |
| **Default username** (bottom of form) | `student` |
| **Default password** (bottom of form) | `Changeme123!` |
| **Generalize method** | cloud-init clean — **only after** you install cloud-init (step 3) |

**Steps**

1. Fill the form per the table and click **Create draft**, then **Provision**.
   When state reaches `configuring`, click **Open Build Console ↗**.
2. Click through the Debian installer. Choose **not** to set a root password
   (leave it blank so `sudo` is used), and create the user **`student`** with
   **`Changeme123!`**. Let it finish and reboot.
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
| **Disk (GB)** | `50` (40–60 is fine) |
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

> [!important]
> **The per-clone password just works — do not set it by hand.** The answer file
> stores the build password in Windows' encoded form
> (`base64(UTF-16LE(cleartext + "Password"))`) in the `oobeSystem` pass, which
> survives sysprep, and cloudbase-init re-applies a **fresh random password per
> student** on first clone boot. Never type a password into a plaintext field or
> edit it in the XML — the pipeline substitutes the per-clone value for you.

> [!important] The blank Windows VM Crucible builds has **no vTPM and no Secure
> Boot** (it's a pvscsi + EFI shell). That's fine for Windows 10 and Server, but
> Windows 11 refuses to install without them — see recipe 6 and the engineering
> note `docs/architecture/iso-build-hardware-gaps.md` for the supported
> workaround.

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
> `windowsPE` injection or the **Load driver** fallback to point at. Until the
> pipeline stages the pvscsi driver (see **Gap B** in
> `docs/architecture/iso-build-hardware-gaps.md`), the reliable fix for a Server
> build that can't see its disk is to give the shell an **LSI SAS** controller
> (which *does* have an in-box Windows driver). Coordinate that with an admin —
> it is a code/shell change, not a wizard field.

<a id="fully-patch-before-sysprep"></a>
> [!warning]
> **Windows 11 24H2 and Server 2025 (build 26100): fully patch BEFORE you
> Generalize.** Un-patched build-26100 images hit a Microsoft shell bug where
> `explorer.exe` crashes with `0xc0000409` on the **first clone boot after
> sysprep /generalize**, leaving a gray screen. This is a Microsoft bug, **not** a
> Crucible defect. Fix: in the Build Console, install the **November 2024 (or
> later) cumulative update** so the image is build **≥ 26100.2314**, *then*
> Generalize. Win11 23H2 (build 22631) is unaffected. (Confirm the exact KB on
> the Windows Update history page for your build.)

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
| **Disk (GB)** | `70` (60–80 is fine) |
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
| **Guest OS ID** | `windows11_64Guest` |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `90` (80–100; Win11 wants ≥ 64 GB) |
| **Firmware / vTPM** | EFI, **no vTPM** — this is exactly why the bypass is required |
| **Unattended → Hostname** | `win11-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `Changeme123!` |
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

> [!note]
> **Where this snippet lives.** Crucible generates the Windows answer file for
> you today, and it does **not** yet include this `windowsPE` block (the
> generator only writes the `specialize` and `oobeSystem` passes). Until it does,
> a Windows 11 ISO build needs this block added to the generated
> `autounattend.xml`, and the file must sit at the **root** of the removable/seed
> volume. This is tracked as **Gap A** in the engineering note
> `docs/architecture/iso-build-hardware-gaps.md` — read it before attempting a
> Win11 template, and coordinate with an admin so the seed disc carries the
> bypass. The alternative long-term fix (give the shell a real vTPM + Secure
> Boot) is **Gap A option (ii)** in that note.

**Steps**

1. Fill the form per the table and click **Create draft**.
2. Ensure the generated `autounattend.xml` includes the `windowsPE` LabConfig
   block above (see the note — this may need admin help until the generator adds
   it).
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

> [!warning]
> **Storage-driver caveat.** Server 2016 Setup has no in-box pvscsi driver, so it
> may show *"We couldn't find any drives"* on Crucible's pvscsi shell — this is
> the same universal issue described in
> [the Windows storage-driver note](#windows-storage-driver-pvscsi). Because the
> pipeline mounts no VMware Tools CD, the practical fix for a Server build is the
> **LSI SAS** controller (**Gap B** in
> `docs/architecture/iso-build-hardware-gaps.md`), which is an admin/code step,
> not a wizard field. If a Tools CD *is* mounted, its pvscsi driver for this OS is
> in `…\pvscsi\Win8\amd64`.

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Windows` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Windows Server 2016 x64 ISO (e.g. `WinServer2016_x64.iso`) |
| **Install mode** | `windows_autounattend` |
| **Guest OS ID** | `windows9Server64Guest` (Windows Server 2016; shares the Windows 10 kernel, so the ID says "windows9Server") |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `70` (60–80 is fine) |
| **Firmware / vTPM** | EFI (default). No vTPM required. |
| **Unattended → Hostname** | `ws2016-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `Changeme123!` |
| **Generalize method** | sysprep /generalize (automatic) |

**Steps** — same as Windows 10 (Provision → configure → Generalize → Publish),
but if Setup can't see the disk, stop and arrange the LSI SAS controller fix
from Gap B before retrying.

> [!note]
> If your vCenter's **Guest OS ID** dropdown doesn't list `windows9Server64Guest`,
> `windows2019srv_64Guest` (Server 2019's ID) is the nearest alternative — the
> install still works; only optimization hints differ.

**You're done when…** Setup finds the disk, completes unattended, and the
template reaches `active`. See the shared
[health-check checklist](#health-check-checklist-every-recipe).

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
| **Disk (GB)** | `70` (60–80 is fine) |
| **Firmware / vTPM** | EFI (default). No vTPM required. |
| **Unattended → Hostname** | `ws2019-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `Changeme123!` |
| **Generalize method** | sysprep /generalize (automatic) |

**Steps** — identical to Windows 10: Provision → (optional) configure →
Generalize → Publish. If Setup shows no disk, apply the Gap B controller fix from
[the Windows storage-driver note](#windows-storage-driver-pvscsi) (Tools pvscsi
driver for WS2019 is `…\pvscsi\Win8\amd64`).

**You're done when…** the template reaches `active` and a test deploy logs in as
`Student` with the per-pod password. See the shared
[health-check checklist](#health-check-checklist-every-recipe).

---

### 9. Windows Server 2022

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Windows` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Windows Server 2022 x64 ISO (e.g. `WinServer2022_x64.iso`) |
| **Install mode** | `windows_autounattend` |
| **Guest OS ID** | `windows2019srvNext_64Guest` ⚠️ ("2019**Next**" = Server 2022, **not** 2019) |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `70` (60–80 is fine) |
| **Firmware / vTPM** | EFI (default). No vTPM required for the base install. |
| **Unattended → Hostname** | `ws2022-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `Changeme123!` |
| **Generalize method** | sysprep /generalize (automatic) |

**Steps** — identical to Windows 10: Provision → (optional) configure →
Generalize → Publish. If Setup shows no disk, apply the Gap B controller fix from
[the Windows storage-driver note](#windows-storage-driver-pvscsi) (Tools pvscsi
driver for WS2022 is `…\pvscsi\Win10\amd64`).

**You're done when…** the template reaches `active` and a test deploy logs in as
`Student` with the per-pod password. See the shared
[health-check checklist](#health-check-checklist-every-recipe).

---

### 10. Windows Server 2025

| Wizard field | Value |
|--------------|-------|
| **OS family** | `Windows` |
| **Source type** | `ISO install` |
| **ISO** | Pick the Windows Server 2025 x64 ISO (e.g. `WinServer2025_x64.iso`) |
| **Install mode** | `windows_autounattend` |
| **Guest OS ID** | `windows2022srvNext_64Guest` ⚠️ ("2022**Next**" = Server 2025, **not** 2022) |
| **vCPUs** | `2` |
| **RAM (MB)** | `4096` |
| **Disk (GB)** | `70` (60–80 is fine) |
| **Firmware / vTPM** | EFI (default). No vTPM required. |
| **Unattended → Hostname** | `ws2025-lab` (or blank) |
| **Unattended → Username** | `Student` |
| **Unattended → Password** | `Changeme123!` |
| **Generalize method** | sysprep /generalize (automatic) |

**Gotchas**

- **Fully patch before you Generalize.** Server 2025 is build **26100** and hits
  the same first-clone gray-screen bug as Windows 11 24H2. Install the Nov 2024+
  cumulative update (build ≥ 26100.2314) in the console **before** Generalize —
  see [fully patch before sysprep](#fully-patch-before-sysprep).
- **BitLocker cmdlets are missing by default.** Crucible's generalize step tries
  to auto-decrypt any BitLocker volume, but Server 2025 doesn't ship
  `manage-bde` / the BitLocker PowerShell module out of the box, so that step
  simply **no-ops** — which is fine for a lab image that never enabled
  BitLocker. Don't be alarmed if the generalize log mentions skipping it.
- **Large cumulative updates can fail over COM.** If a big cumulative update
  errors out when scripted, drive it from **Settings → Windows Update** (or
  `UsoClient StartInstall`) rather than a scripted COM call, which can fail on
  WS2025.
- Storage: if Setup can't see the disk, apply the Gap B controller fix from
  [the Windows storage-driver note](#windows-storage-driver-pvscsi) (Tools pvscsi
  driver for WS2025 is `…\pvscsi\Win10\amd64`).
- **Guest OS ID fallback:** if your ESXi 8.0 build's dropdown doesn't offer
  `windows2022srvNext_64Guest` yet, fall back to `windows2019srvNext_64Guest`
  (Server 2022's ID) — the install still works; only optimization hints differ.

**You're done when…** the template reaches `active`; a test clone boots to a
desktop (no gray screen) and the generalize log skipping BitLocker is expected,
not an error. See the shared
[health-check checklist](#health-check-checklist-every-recipe).

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
   confirm the pod page shows a **random** password (not `Changeme123!`).
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
