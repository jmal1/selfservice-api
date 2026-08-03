# Building a Template

> [!note]
> Templates are the **frozen VM images** every lab pod is cloned from.
> See [overview.md](overview.md) for how templates fit into the bigger
> picture (template → blueprint → pod → playlist → workflow → action).

This page walks through the **template-creation wizard** end-to-end, with
special attention to the **browser-based build console** — you do not need
a vCenter account to build a template.

---

## Lifecycle in one diagram

```
   draft ──Provision──▶ provisioning ──auto──▶ configuring ──Generalize──▶ generalizing ──auto──▶ ready ──Publish──▶ verifying ──auto──▶ active
                                                  ▲                                                    │                    │
                                                  └────────────── Reconfigure ─────────── Unpublish ◀──┘                    │
                                                                                                       ▲── smoke test fails ─┘
```

Every state except `draft` has a **staging VM** living in vCenter's
Templates folder. The wizard auto-refreshes; you don't need to watch
the page.

| State | What's happening | Console available? |
|-------|------------------|--------------------|
| `draft` | Metadata only, no VM yet | No |
| `provisioning` | Worker is cloning your source VM (5–10 min) | **Yes** (VM may not have power immediately) |
| `configuring` | VM is up; **install + configure your software here** | **Yes — the main reason to open it** |
| `generalizing` | Sysprep / cloud-init clean is running (2–5 min) | **Yes** (useful to watch progress) |
| `ready` | Template is generalized and ready to publish | No (staging VM cleaned up) |
| `verifying` | **Automated smoke test** — Crucible clones a throwaway pod, boots it unattended, confirms it comes up, then destroys it | No |
| `active` | Published — students can launch pods from it | No |
| `error` | A worker job failed; check Last error in the wizard | No |

> [!note]
> **Publish is gated by a smoke test.** When you click Publish, the
> template first enters `verifying`: Crucible clones a disposable VM
> from the freshly-generalized image, powers it on, and waits for it to
> boot unattended (VMware Tools + an IP lease). If it boots cleanly the
> template auto-advances to `active`; if it fails to boot the template
> returns to `ready` with the failure recorded — so a bricked image can
> never reach students. The throwaway VM is always cleaned up.

---

## Step 1 — Draft

Go to **Admin → Templates → New**. Fill in:

- **Name** — human-friendly, will be shown to students
- **OS type** — `ubuntu`, `windows`, `kali`, etc. (drives the generalize behavior)
- **Source** — clone from an existing Crucible template, or paste a vCenter VM moref
- **Staging network** — defaults to `PG-VM-Lab` (the VLAN 30 port group on every host); only change if you know why

Click **Create draft**. You'll land on the wizard page for the new template.

## Step 2 — Provision

Click **Provision**. The worker clones the source VM into the
Templates folder, attaches a NIC on the staging network, powers it on,
and waits for VMware Tools. This is the slow step.

As soon as state flips to `configuring`, the **Open Build Console**
button appears.

### Provisioning from an ISO

If the source is an **ISO** rather than an existing VM, Provision builds a
blank VM, attaches the installer ISO, and boots it. What happens next depends
on the template's **unattended install mode**:

| Mode | What Provision does | How long |
|------|--------------------|----------|
| `manual` | Boots the installer and stops. **You install the OS yourself** through the Build Console, then click Generalize. | Up to you |
| `cloudinit_cidata` (Ubuntu Server) | Attaches a generated cloud-init seed CD and runs a **hands-off** autoinstall. No console input required. | 20–45 min |
| `windows_autounattend` | Attaches a generated `autounattend.xml` seed CD and runs Windows Setup unattended. | 30–60 min |

> [!note]
> **An unattended install finishes when the VM powers itself off.**
> The generated config ends with `shutdown: poweroff`, and the worker waits
> for that — not for VMware Tools. This matters because the Ubuntu Server
> installer runs VMware Tools *inside the installer environment*, roughly 40
> seconds after power-on and long before anything is written to disk. Tools
> appearing early is normal and is not a sign the install is done.
>
> Once the VM powers off, the worker detaches both CDs, boots the installed
> system, waits for *its* Tools, and moves the template to `configuring`.

> [!warning]
> **Ubuntu autoinstall needs the confirmation prompt answered.** The Ubuntu
> installer refuses to touch the disk until someone confirms, and normally
> that requires an `autoinstall` kernel argument we cannot add from a seed CD.
> Crucible answers the prompt automatically from inside the installer. If you
> open the console and see `Continue with autoinstall? (yes|no)` sitting there
> for more than a couple of minutes, that automation failed — answer `yes` to
> unblock this build and report it, because every future build of that ISO
> will stall the same way.

Progress messages in the wizard tell you which phase you're in:
`create_vm` → `power_on` → `wait_install` → `detach_cdrom` → `boot_installed`
→ `wait_tools` → `configuring`.

## Step 3 — Configure (the new part)

> [!tip]
> Click **Open Build Console ↗** in the wizard. A new tab opens with a
> browser-native VM console — same as the pod-VM consoles students use.
> No vCenter login, no VMware Remote Console install, no port forwarding.

Inside the console you can:

- Type and click into the guest OS exactly like a vCenter Web Console
- **Paste from clipboard** (`Ctrl+Shift+V` or the 📋 Paste button)
- Use **Text Input** drawer if your browser blocks clipboard access
- Send **Ctrl+Alt+Del**

Do whatever you need to: install software, harden the OS, drop in
configuration files, create user accounts. Save your work *inside the
guest* (the snapshot is taken when you click Generalize, not before).

When you're done:

1. Close the console tab.
2. Back in the wizard, fill in **Guest username + password** (these are
   the credentials Crucible will use over VMware Guest Operations to
   run the sysprep script).
3. Click **Generalize**.

## Step 4 — Generalize

Crucible runs the OS-appropriate cleanup (`cloud-init clean
--logs --seed` on Linux, `sysprep /generalize` on Windows), takes the
initial snapshot, and powers the VM down. You can leave the console
open to watch it happen.

When state flips to `ready`, the staging VM is no longer interactive —
the console button disappears.

You are not asked for the guest's username and password. Crucible uses, in
order: whatever you supply explicitly, then the template's
`default_username` / `default_password`, then — for a template built by an
**unattended ISO install** — the account the installer created, which the
platform generated and recorded when it built the seed ISO. That last case
matters because those credentials are not displayed anywhere in the wizard.

> [!note]
> **Reaching `ready` means the cleanup provably finished**, not just that the
> VM powered off. On Linux the script stamps a completion marker that survives
> the shutdown; if it is missing, the template goes to `error` rather than
> `ready` even though the VM is off. See
> [Generalize failed, or the template published but clones behave oddly](troubleshooting.md#generalize-failed-or-the-template-published-but-clones-behave-oddly)
> for what each outcome means.

## Step 5 — Publish

Click **Publish to students**. The template first enters `verifying`,
where Crucible runs an automated smoke test (clone a throwaway pod →
boot it unattended → confirm it comes up → destroy it). If it passes,
the template becomes `active` and visible in the public catalog. If the
smoke test fails, the template drops back to `ready` with the failure
recorded — fix the image and Publish again. **Unpublish** an active
template at any time to hide it without losing the generalized image.

---

## Linux template contract

> [!danger]
> **A Linux template that violates this contract still clones and boots —
> the student just silently cannot log in.** There is no error in the
> wizard, no failed job, no alert. The pod shows "running" and green, and
> the failure surfaces only when the student tries to SSH or open the
> console and their password is refused. Get these five things right
> *before* you Generalize.

Crucible clones your template per student and injects a **unique per-pod
password** over VMware guestinfo. That injection only works if the guest
image satisfies the contract below. The publish gate validates the parts
it can see from the template row (e.g. a blank `default_username`), but
the guest-internal pieces are yours to get right inside the build console.

### The five requirements

**1. open-vm-tools installed and running.** Without it the guestinfo
payload Crucible writes is never read, so the password is never applied.

```bash
sudo apt-get install -y open-vm-tools
sudo systemctl enable --now open-vm-tools
command -v vmware-rpctool   # must print a path
```

**2. cloud-init ≥ 21.3 with the VMware datasource enabled.** Crucible's
customization is delivered through cloud-init's VMware datasource. Create
`/etc/cloud/cloud.cfg.d/99-crucible.cfg` with exactly:

```yaml
datasource_list: [ VMware, NoCloud, None ]
system_info:
  default_user:
    name: student
    sudo: ["ALL=(ALL) NOPASSWD:ALL"]
    shell: /bin/bash
```

> [!danger]
> **The cloud-init default user MUST be `student`.** Crucible writes a
> bare top-level `password:` into the `#cloud-config` payload, and
> cloud-init applies that password to the **default user only**. Crucible
> then tells the student to log in as `student`. If your default user is
> `ubuntu`, `admin`, or anything else, the injected password lands on an
> account the student is never told about — the student's `student` login
> has no password and is refused. This is the most common silent failure,
> and it is exactly what the publish gate flags when `default_username`
> is empty or is set to something other than `student` on a customized
> template.

**3. SSH host keys must regenerate after generalize.** Generalize runs
`rm -f /etc/ssh/ssh_host_*`. If nothing regenerates them on next boot,
sshd fails its config test and the student cannot SSH in.

> [!danger]
> **Ubuntu 24.04 does NOT ship `ssh-keygen.service`.** Earlier versions of
> this page said it did. Verified on a real 24.04.3 build:
> `systemctl is-enabled ssh-keygen.service` returns **not-found**, ssh is
> socket-activated through `ssh.socket`, and `ssh.service` only runs
> `sshd -t`. cloud-init's `ssh` module *will* eventually recreate missing
> host keys on a new instance, but it races socket activation — so a clone
> can refuse connections until cloud-init catches up. **Install the unit
> below; do not assume the distro does this for you.**

```ini
# /etc/systemd/system/crucible-regen-ssh-hostkeys.service
[Unit]
Description=Regenerate missing OpenSSH host keys (Crucible)
ConditionPathExists=!/etc/ssh/ssh_host_ed25519_key
Before=ssh.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/ssh-keygen -A

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable crucible-regen-ssh-hostkeys.service
```

The `ConditionPathExists` guard makes it a no-op on every later boot, so
it can never rotate a running pod's host key out from under an open
session.

> [!danger] Order before `ssh.service`, **never** before `ssh.socket`.
> Adding `ssh.socket` to that `Before=` line looks stricter and is in fact a
> **systemd ordering cycle** that disables SSH entirely. Because the unit is
> `WantedBy=multi-user.target` it inherits `After=basic.target`, and
> `basic.target` is ordered after `sockets.target`, which is ordered after
> `ssh.socket`. systemd breaks the loop by *deleting a job* — and on a real
> build it deleted `ssh.socket/start`, so nothing ever bound port 22 on the
> template or on any clone made from it. The boot log is the giveaway:
>
> ```
> sockets.target: Found ordering cycle on ssh.socket/start
> sockets.target: Job ssh.socket/start deleted to break ordering cycle
> ```
>
> Note the `ConditionPathExists` guard does **not** protect you here: systemd
> resolves ordering cycles *before* it evaluates conditions, so a unit that
> would have been skipped anyway still takes SSH down.
>
> Ordering before `ssh.service` is both cycle-free and sufficient. Under socket
> activation `ssh.socket` only binds the port; `ssh.service` is what execs
> `sshd` and reads the host keys. If a connection arrives before this unit has
> run, systemd simply orders `ssh.service` after it, so `sshd` still never
> starts without keys.

> [!note]
> On **Ubuntu 24.04** the SSH daemon unit is `ssh.service`, **not**
> `sshd.service`, and it is socket-activated. Order host-key regeneration
> `Before=ssh.service ssh.socket` — ordering before `ssh.service` alone is
> not enough when the socket accepts the connection first.

**4. apt proxy pointed at the staging cache.** So package installs during
build go through the lab's apt-cacher-ng. Create
`/etc/apt/apt.conf.d/01proxy`:

```
Acquire::http::Proxy "http://10.10.30.20:3142";
```

> [!warning]
> Point **only** the http proxy at the cache. apt-cacher-ng is an HTTP
> cache; routing `Acquire::https::Proxy` through it breaks https
> repositories rather than caching them.
>
> On an ISO-built template the installer writes this file itself, as
> `/etc/apt/apt.conf.d/90curtin-aptproxy`. Check that path too, and check
> its **value** — a malformed proxy URL there is not a syntax error, so
> apt only fails later, at every fetch.

**5. The build user needs passwordless sudo.** Generalize runs
`sudo cloud-init clean`, `sudo truncate -s 0 /etc/machine-id` and
`sudo rm -f /etc/ssh/ssh_host_*` through VMware guest ops — with **no
tty**, so a password prompt cannot be answered and the whole script
aborts.

> [!danger]
> **Being in the `sudo` group is not enough, and the `sudo:` line in
> `99-crucible.cfg` does not grant this.** cloud-init only applies
> `system_info.default_user` when it *creates* the account. If the account
> already exists — which it does on any ISO install, because the installer
> created it — that block is ignored, the user lands in group `sudo`, and
> Ubuntu's stock `%sudo ALL=(ALL:ALL) ALL` requires a password.

```bash
echo 'student ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/90-crucible-student
sudo chmod 0440 /etc/sudoers.d/90-crucible-student
sudo visudo -cf /etc/sudoers.d/90-crucible-student   # must print "parsed OK"
sudo -n true && echo OK                              # the real test
```

The `chmod 0440` is not cosmetic: **sudo silently ignores a drop-in that
is group- or world-writable** and tells you nothing. Always finish with
`sudo -n true`.

> [!tip]
> **A `cloudinit_cidata` ISO build does all five of these for you.** If you
> provision from an Ubuntu Server ISO with `unattend_mode=cloudinit_cidata`,
> the generated autoinstall installs open-vm-tools and cloud-init, writes
> `99-crucible.cfg`, installs and enables the host-key regeneration unit,
> configures the apt proxy, and drops in passwordless sudo. Run the
> verification block below anyway — it is cheap, and it is the only thing
> that proves it.

> [!warning]
> **Linux Mint does not ship cloud-init.** A Mint template will never
> receive the injected password through the flow above. Either install
> and configure cloud-init (requirements 1–2) so it behaves like Ubuntu,
> **or** mark the template as not-customizable (`clone_no_customize` /
> `registered_existing_vm`) and set static `default_username` /
> `default_password` on the template row so Crucible surfaces real,
> working credentials to the student. A customized Mint template with no
> cloud-init is a guaranteed silent lockout.

### Verify before publishing

Open the build console, log in, and run these inside the guest. All five
must look right before you Generalize:

```bash
command -v vmware-rpctool                            # open-vm-tools present (req 1)
cloud-init --version                                 # must be >= 21.3 (req 2)
cat /etc/cloud/cloud.cfg.d/99-crucible.cfg           # default_user.name: student (req 2)
systemctl is-enabled crucible-regen-ssh-hostkeys     # must be "enabled" (req 3)
systemctl is-active ssh.socket                       # must be "active" - see the ordering-cycle warning (req 3)
journalctl -b | grep 'ordering cycle on ssh.socket'  # must print NOTHING (req 3)
grep -rh -i proxy /etc/apt/apt.conf.d/               # a bare URL, not a dict (req 4)
sudo -n true && echo "sudo OK"                       # must print sudo OK (req 5)
```

> [!danger]
> **`sudo -n true` is the single most important line here.** It is the only
> one that fails the way generalize fails: no tty, no prompt, non-zero
> exit. If it does not print `sudo OK`, Generalize will abort partway and
> leave the template with a populated `/etc/machine-id` and the original
> SSH host keys — which means every clone shares them.

Also confirm the template row's **default_username is `student`** (for a
customized template) or holds real static credentials (for a
non-customized one). The wizard's publish gate blocks a blank or
non-`student` username on a customized Linux template for exactly this
reason.

---

## Console access rules

Who can open the build console for a given template?

- The user who **created** the template (the `created_by` value), OR
- Any **admin**

Which lifecycle states allow console access?

- `provisioning`, `configuring`, `generalizing` — anywhere a real
  staging VM exists in vCenter

If you hit a `409 Conflict` when opening the console, the template has
already moved past the interactive phase; refresh the wizard to see
its current state.

---

## Troubleshooting

> [!warning]
> **Console shows "Connecting…" indefinitely.** The staging VM might
> not have power yet (early `provisioning`) or it might have just
> rebooted. Click **Reconnect** in the console toolbar after ~30 seconds.

> [!warning]
> **Generalize fails.** Open the console, log in with the credentials
> you provided, and check `/var/log/cloud-init.log` (Linux) or
> `C:\Windows\System32\Sysprep\Panther\setupact.log` (Windows). The
> wizard's **Last error** field also shows the worker's report.

> [!note]
> **The console disconnects after sysprep runs.** Expected — sysprep
> reboots the guest, which kills the WebMKS session. The wizard
> auto-advances to `ready` once the worker confirms the snapshot was
> taken.

See [troubleshooting.md](troubleshooting.md) for more general help.

---

## Related pages

- [overview.md](overview.md) — How templates fit into pods, blueprints, playlists
- [glossary.md](glossary.md) — Definitions of `template`, `blueprint`, `pod`, etc.
- [troubleshooting.md](troubleshooting.md) — Common provisioning failures
