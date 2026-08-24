# Building a Template

Templates are the **frozen VM images** every lab pod is cloned from. See
[overview.md](overview.md) for how templates fit into the bigger picture
(template → blueprint → pod → playlist → workflow → action).

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

**Publish is gated by a smoke test.** When you click Publish, the template
first enters `verifying`: Crucible clones a disposable VM from the
freshly-generalized image, powers it on, and waits for it to boot unattended
(VMware Tools + an IP lease). If it boots cleanly the template auto-advances to
`active`; if it fails to boot the template returns to `ready` with the failure
recorded, so a bricked image cannot reach students. The throwaway VM is always
cleaned up.

---

## Step 1 — Draft

> [!tip] New here? Read this whole section before you touch the form.
> It tells you **exactly what to type in every box**. You do not need to
> understand VMware to build a template — just follow the recipe for your OS.

Go to **Admin → Templates → New**. The form has four boxes-worth of fields:
**Identity**, **Source**, **Hardware**, and **Guest credentials**. The tables
below say what to put in each. When in doubt, copy the recipe.

### Words you'll see (quick glossary)

- **Template** — a finished, frozen VM image. Every student pod is a fresh copy
  ("clone") of a template.
- **ISO** — the installer disc file for an operating system (what you'd burn to
  a USB stick to install Windows or Ubuntu). Ends in `.iso`.
- **Provision** — the wizard builds the working VM you'll set up.
- **Generalize** — the wizard cleans the VM so every student copy is unique.
- **Unattended install** — the OS installs itself with no clicking, using
  answers you fill in here.

### Quick-start recipes (copy these)

For a full click-by-click recipe for one OS, see
[Per-OS Template Build Recipes](os-recipes.md). It covers Ubuntu
Server/Desktop, Debian, Linux Mint, Windows 10/11, and Windows Server
2016/2019/2025, including the exact Guest OS ID and the Windows 11
TPM/Secure-Boot bypass. The table below is the one-line summary.

Pick the row that matches what you're building and use exactly those values.
Anything not listed, **leave blank** — the wizard fills in sensible defaults.

| I want to build… | Source type | ISO install mode | Username | Password | Everything else |
|------------------|-------------|------------------|----------|----------|-----------------|
| A copy of an existing lab template (most common) | **Clone an existing Crucible template** | — | leave blank | leave blank | leave blank |
| Ubuntu Server from scratch | **ISO install** | `cloudinit_cidata` | `student` | `Changeme123!` | leave blank |
| Kali or Debian from scratch | **ISO install** | `debian_preseed` | `student` | `Changeme123!` | leave blank |
| Windows from scratch | **ISO install** | `windows_autounattend` | `Student` | `Changeme123!` | leave blank |
| A desktop OS I want to click through myself | **ISO install** | `manual` | leave blank | leave blank | leave blank |

The **standard build password** for this lab is `Changeme123!`. Use it wherever
the wizard asks you to make up a password, unless your teacher tells you
otherwise. See
[The standard build login](#the-standard-build-login-studentchangeme123) for
what this password is and is not.

### Field reference — Identity

| Field | What to type | Default | Why it matters |
|-------|--------------|---------|----------------|
| **Template name** *(required)* | A friendly name students will see, e.g. `Ubuntu 24.04 — Web Security` | — | This is the label in the catalog. Make it clear. |
| **OS family** *(required)* | `Linux` or `Windows` | `Linux` | Controls how the wizard cleans the image (cloud-init vs. sysprep). Pick the one that matches your ISO/source. |
| **Staging network** | *(nothing — it's fixed)* | `PG-VM-Lab` | Not editable. Every build VM is forced onto the isolated `PG-VM-Lab` (VLAN 30) network so a half-built image can never touch the real lab network. This is a security control baked into every template — the wizard just shows it read-only. |
| **Description** | One or two sentences on what's inside | — | Shown to students. Optional but kind. |
| **Icon URL** | A link to a logo image, or leave blank | — | Cosmetic only. |

### Field reference — Source

**Source type** tells the wizard where the VM comes from. Pick one:

| Source type | Choose this when… | What goes in the box below |
|-------------|-------------------|----------------------------|
| **Clone an existing Crucible template** | You want to start from a template that already works (fastest, safest) | Pick a template from the dropdown |
| **Clone an existing vCenter VM** | A teacher/admin points you at a specific VM or an imported OVA | Pick the VM from the dropdown |
| **ISO install** | You're installing an OS from scratch off an installer disc | Pick your `.iso` from the dropdown |

> [!warning]
> Before you click **Provision** for an existing Windows VM or template with a
> vTPM, verify BitLocker is fully decrypted and protection is off. Provisioning
> replaces the source vTPM identity, so a staging clone cannot unlock a volume
> that still depends on the source TPM.

If an ISO is not in the list, upload it on the **Images** page and wait for the
import to finish. It then appears under "Uploaded & imported ISOs." ISOs already
on the server appear under "ISOs already on vCenter datastore." The value looks
like `[NAS-BackupsAndISOS] ISOs/ubuntu-24.04.iso`; choose it rather than typing
it.

### Field reference — Hardware

Starting size for the build VM. Students can bump CPU/RAM later within their quota.

| Field | Safe starting value | Notes |
|-------|--------------------|-------|
| **vCPUs** | `2` | Range 1–16. |
| **RAM (MB)** | `4096` (= 4 GB) | Minimum 512. Windows wants at least `4096`. |
| **Disk (GB)** | `40` | Minimum 10. Give Windows `60`+. A clone keeps this size. |

### Field reference — Guest credentials (bottom of the form)

These are the **username and password the wizard uses to log in and clean the
VM** during Generalize.

| Field | What to type | If you leave it blank |
|-------|--------------|-----------------------|
| **Default username** | The account you'll log in as. For a **Linux** template this **must be** `student`. For Windows, `Student`. | For "Clone an existing Crucible template", it's copied from the source template. For an ISO build, the account you created during install is used. |
| **Default password** | The standard build password `Changeme123!` | Same fallback as above. |

> [!warning] For **Linux** templates the username has to be exactly `student`.
> Crucible gives every student pod its own password by setting it on the account
> named `student`. If your account is called `ubuntu` or `admin`, students get a
> password on an account they're never told about and **can't log in**. The
> publish step blocks this, but save yourself the round-trip: use `student`.

---

### ISO install — the "Unattended install" fields

*(Only appears when Source type = **ISO install**.)*

An unattended install means the OS installs itself using answers you provide,
with no clicking. Choose an **Install mode** that matches your ISO:

| Install mode | Use it for | Hands-off? | Roughly how long |
|--------------|-----------|------------|------------------|
| `manual` | Any OS you'd rather click through yourself in the console (desktop Linux, odd distros) | No — you drive the installer | Up to you |
| `cloudinit_cidata` | **Ubuntu Server** ISOs | Yes | 20–45 min |
| `debian_preseed` | **Debian** and **Kali** ISOs | Yes | 20–45 min |
| `windows_autounattend` | **Windows** ISOs | Yes | 30–60 min |

If you pick anything other than `manual`, a few more boxes appear. Here's what
each one wants — **most can be left blank**:

| Field | What to type | Leave blank to get… |
|-------|--------------|---------------------|
| **Hostname** | A computer name like `ubuntu-lab` (letters, numbers, dashes) | A generic name — fine for most labs |
| **Username** | `student` for Linux, `Student` for Windows | `student` |
| **Password** | `Changeme123!` (the standard build password) | *Don't leave blank* — always set a password here |
| **Locale** | Leave blank | `en_US.UTF-8` |
| **Time zone** | Leave blank | `America/New_York` |
| **APT proxy** *(Linux only)* | `http://10.10.30.20:3142` to speed up package downloads | No proxy (installs still work, just slower) |
| **Extra packages** | Comma-separated tools you want pre-installed, e.g. `curl, git, vim` | Nothing extra |

You type the password in plain text here. Crucible stores it safely (encrypted
on Windows, hashed on Linux). This is the password you use to log into the build
VM in the next step.

A `cloudinit_cidata` Ubuntu build sets up VMware Tools, the `student` account,
passwordless sudo, SSH host keys, and the apt proxy automatically. That is why
it is the easiest Linux path; see [Linux template contract](#linux-template-contract)
for the details it handles.

For the full play-by-play of what happens after you click Provision on an ISO,
see [Provisioning from an ISO](#provisioning-from-an-iso).

---

### The standard build login (`Student`/`Changeme123!`)

Whenever the wizard asks you to make up a username/password for the VM you are
building, use **`student`** for Linux or **`Student`** for Windows, with the
password **`Changeme123!`**.

This is the **build login** — the account *you* use to log into the VM in the
console while you set it up. It is a shared, well-known convention so anyone on
the team can pick up a half-built template.

This is **not** the password students get. When a student launches a pod,
Crucible generates a **brand-new random password just for them** and shows it
on their pod page. `Changeme123!` only ever lives on the build VM and is
replaced on every student copy. Never tell a student that their password is
`Changeme123!`; theirs is different.

When do you type it vs. leave things blank?

- **Cloning an existing Crucible template:** leave the credential boxes blank.
  The template you cloned already has working credentials and they carry over.
- **Building from an ISO:** type `Student`/`Changeme123!` in the **Unattended
  install** boxes so the installer creates that account.
- **Any template where you're unsure what the login is:** set the **Default
  username / Default password** at the bottom to `Student`/`Changeme123!` so the
  wizard has something real to log in with during Generalize.

Click **Create draft**. You'll land on the wizard page for the new template.

## Step 2 — Provision

Click **Provision**. The worker clones the source VM into the
Templates folder, attaches a NIC on the staging network, powers it on,
and waits for VMware Tools. This is the slow step.

Before creating anything, Crucible requires one explicitly allowlisted vCenter
host that is compatible with the source/resource pool, connected, outside
maintenance mode, mounted to the target datastore, and carrying the staging
standard portgroup. A **No eligible allowlisted vCenter placement** error is an
operator safety block; do not change the template or broaden the host list to
work around it. See [Troubleshooting](troubleshooting.md#no-eligible-allowlisted-vcenter-placement).

### Source replicas for multiple compute clusters

A logical template needs one real, validated source VM in every compute cluster
where it may be cloned. An administrator registers a source replica by immutable
vCenter identity:

```http
POST /api/v1/admin/templates/{templateID}/source-replicas
Content-Type: application/json

{"source_ref":"vm-123"}
```

Use `GET /api/v1/admin/templates/{templateID}/source-replicas` to inspect
`replica_mode` and the `replicas` array, and
`DELETE /api/v1/admin/templates/{templateID}/source-replicas/{replicaID}` to
remove an unused one. Registration verifies the source VM and its current
compute resource live in vCenter. The database migration does not invent
replicas for another cluster.

The source VM's current host does not need to be in `VCENTER_HOSTS`.
Registration is read-only inventory validation: it records the live VM, host,
and compute-resource identity, but it does not authorize placement or mutation
on that host. Target eligibility remains controlled exclusively by the frozen
`VCENTER_HOSTS` allowlist and compatible configured resource pools. Registering
a source on another cluster is therefore safe bootstrap preparation, not host
admission.

#### Durable retained replica builds

Use the asynchronous build API instead of an operator-side govmomi script when
the target compute resource does not have a retained source yet:

```http
POST /api/v1/admin/templates/{templateID}/source-replica-builds
Content-Type: application/json

{
  "source_replica_id": "11111111-1111-1111-1111-111111111111",
  "idempotency_key": "intel-cluster-retained-v1",
  "destination_name": "ubuntu-24-intel-retained",
  "target": {
    "compute_resource_type": "ClusterComputeResource",
    "compute_resource_moref": "domain-c401",
    "compute_resource_path": "/LAB/host/Intel-Cluster",
    "host_moref": "host-3401",
    "host_name": "nuc3.lab.jmal.io",
    "resource_pool_moref": "resgroup-401",
    "resource_pool_path": "/LAB/host/Intel-Cluster/Resources/Student-VMs",
    "datastore_moref": "datastore-401",
    "datastore_name": "Intel-Templates",
    "folder_moref": "group-v401",
    "folder_path": "/LAB/vm/Templates"
  }
}
```

The caller must send both MoRefs and the matching inventory paths/names.
Crucible resolves and compares every identity before enqueueing. The target host
may be outside `VCENTER_HOSTS`; this endpoint is the only exception and does not
add that host, compute resource, or pool to normal placement eligibility. A
subsequent pod placement on that host still fails the allowlist gate.

The API also checks the vCenter service account's entity-scoped build
privileges. The minimum set is `VirtualMachine.Provisioning.Clone` on the source
VM; `VirtualMachine.Inventory.Create`, `VirtualMachine.Inventory.Delete`,
`VirtualMachine.State.CreateSnapshot`, and
`VirtualMachine.Config.AdvancedConfig` plus
`VirtualMachine.Provisioning.Clone` inherited by the target folder;
`Resource.AssignVMToPool` on the target pool; and
`Datastore.AllocateSpace` on the retained and provisioning datastores.
`Cryptographer.Clone` is additionally required on the source and inherited by
the target folder when the source has a vTPM.
Crucible uses the normal strict vCenter TLS client and does not add a certificate
bypass.

`202 Accepted` means a new durable operation and job were committed together.
Repeating the same template ID, idempotency key, and immutable request returns
the original operation with `200 OK`; reusing the key with different input
returns `409 Conflict`. Stale inventory, missing privilege, or an inaccessible
destination returns `422 Unprocessable Entity` before vCenter mutation.

```http
GET  /api/v1/admin/templates/{templateID}/source-replica-builds/{buildID}
POST /api/v1/admin/templates/{templateID}/source-replica-builds/{buildID}/retry
POST /api/v1/admin/templates/{templateID}/source-replica-builds/{buildID}/cleanup
```

The status response exposes the durable `status`, `phase`, task MoRefs, exact VM
MoRefs, cleanup timestamps, and `last_error_code`/`last_error`. `retry` is
accepted only for a recoverable `failed` build and resumes its persisted phase.
`cleanup` is accepted for a failed or `cleanup_required` build and performs
marker- and exact-MoRef-based residue cleanup. There is intentionally no broad
delete endpoint. Never delete a same-name VM manually until the stored markers,
MoRefs, and task history have been compared; a collision or ambiguous lineage
fails closed.

The worker persists each submission phase before calling vCenter. If a clone,
snapshot, or cleanup response is lost, a successor reconciles the persisted task
or exact operation marker. Clone, snapshot, and linked-clone creation are never
blindly resubmitted. An exact-VM destroy may be resubmitted only after the
successor revalidates the MoRef and build/operation/kind markers; this closes the
arm-before-RPC crash window without permitting wrong-object deletion. The retained
clone is a powered-off full clone of the source's `base-image` snapshot using
`moveAllDiskBackingsAndDisallowSharing`. A vTPM source uses
`TpmProvisionPolicy=replace`; the build requires distinct public EK
certificate/CSR hashes, but deliberately neither rekeys nor requires a distinct
configuration encryption key.

Before sealing, Crucible verifies source snapshot identity, exact destination
compute/pool/host/datastore/folder, powered-off state, firmware, Secure Boot,
vTPM count, security provider, non-empty configuration key, independent disk
backings, and absence of snapshots or attached ISOs. It then creates the
destination `base-image` snapshot and proves that a powered-off linked clone can
be created on the configured provisioning datastore with an exact retained
parent backing. The canary is never booted. Its exact cleanup must finish and
leave no marked residue before one transaction rechecks the original ready
anchor and promotes the pending replica plus build to `ready`. This acceptance
does not establish guest or L1 health.

If acceptance never reaches `ready`, successful exact retained-VM cleanup
atomically records `residue_cleaned_at` and removes only that build's non-ready
source-replica reservation. The cleaned operation cannot resume forward work;
submit a new build with a new idempotency key to reuse that compute resource.
Template deletion returns `409` before destroying its staging VM while any
replica build is active, accepted, or still owns an uncleaned VM/reservation.

Do not start one of these builds while provisioning worker claims are disabled:
`template_replica_build` is a clone-capable job and remains pending under
`WORKER_PROVISIONING_CLAIMS_ENABLED=false`.

A template keeps using its existing source for backward compatibility only
until the first replica is registered. Registration durably enables
source-replica mode; deleting every replica does not restore the legacy
fallback. An enabled template with no `ready` replica fails closed until a
replacement is registered. Provisioning requires the source, resource pool, and
selected host to belong to the same compute resource. Each pod's complete
placement is stored before host networking changes begin, and retries reuse
that exact source, pool, and host. Host headroom admission also includes durable
RAM reservations from other unreleased placement plans under a per-host
PostgreSQL lock, so concurrent workers cannot each spend the same free memory.
A reservation is released only after its VM is running or exact cleanup proves
that no VM remains; a failed/manual-cleanup job does not release capacity by
status alone. An already-resident VM is not charged twice during legacy
recovery.

For clustered destinations, Crucible disables automatic DRS movement for each
created VM and detects later host, compute, or DRS-control drift before forward
power and snapshot operations. Standard-switch portgroups are created only on
the union of hosts selected for that pod. Adding a VM is limited to hosts already
covered by the pod's durable portgroup receipt. A confirmed identity/control
mismatch requires manual cleanup; a timeout, connection failure, or unreadable
vCenter property remains an operational error eligible for normal retry.
Cleanup also rechecks the live resource pool and the durable source, replica,
template, pod-VM, compute, pool, and host clone markers before deletion. If a
clone failed before its first configuration installed the DRS override, cleanup
installs and verifies that control before deletion; an enabled override remains
drift and fails closed.

> [!note] The periodic template-health and L1 views are still logical-template
> views in this foundation. Registration and placement validate each replica
> live, but the UI does not yet show an independently confirmed health history
> per replica.

#### Replica-build Prometheus and Grafana contract

The pipeline exporter publishes:

| Metric | Contract |
|---|---|
| `crucible_template_replica_build_total{result}` | Build job attempts by `success` or `error`; transient reconciliation attempts may increment `error`. |
| `crucible_template_replica_build_duration_seconds_{sum,count}{result}` | Attempt wall time by result. |
| `crucible_template_replica_build_phase{phase}` | PostgreSQL-backed count of active operations in each durable phase. |
| `crucible_template_replica_build_stuck` | Active operations whose persisted phase has not changed within the worker's staleness threshold. |
| `crucible_template_replica_build_last_success_timestamp_seconds` | Latest successful attempt timestamp. |
| `crucible_template_replica_build_last_failure_timestamp_seconds` | Latest unsuccessful attempt timestamp. |

This repository does not own the live PrometheusRule. Install rules equivalent
to:

```yaml
- alert: CrucibleTemplateReplicaBuildStuck
  expr: crucible_template_replica_build_stuck > 0
  for: 15m
  labels:
    severity: warning
  annotations:
    summary: Durable template replica build is stuck
    description: Inspect the build status API, persisted phase/task MoRefs, provision-worker ownership, and vCenter task history. Do not resubmit or delete by VM name.
    runbook_url: https://github.com/jmal1/selfservice-api/blob/main/docs/instructor/templates.md#durable-retained-replica-builds

- alert: CrucibleTemplateReplicaBuildCleanupRequired
  expr: crucible_template_replica_build_phase{phase="cleanup_required"} > 0
  for: 15m
  labels:
    severity: warning
  annotations:
    summary: Template replica build requires exact cleanup
    description: Use GET to inspect stored ownership, then invoke the operation-scoped cleanup endpoint. Escalate ambiguous markers or lineage rather than deleting a same-name VM.
    runbook_url: https://github.com/jmal1/selfservice-api/blob/main/docs/instructor/templates.md#durable-retained-replica-builds

- alert: CrucibleTemplateReplicaBuildFailures
  expr: increase(crucible_template_replica_build_total{result="error"}[30m]) > 0
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: Template replica build attempts are failing
    description: Correlate last failure time with the build last_error fields and worker logs; distinguish a retryable timeout from collision, source drift, validation failure, or cleanup_required.
    runbook_url: https://github.com/jmal1/selfservice-api/blob/main/docs/instructor/templates.md#durable-retained-replica-builds
```

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

#### Getting an ISO into the picker

Before you can select an ISO in the wizard, it must be **uploaded** through
**Admin → Images → Upload**. After the upload completes, an import job is
enqueued automatically — you do not need to take any further action. The ISO
will appear in the wizard picker with the status `Importing…` until the import
finishes, then it will become selectable.

If the import fails (shown in the image list as an error with a message), click
**Retry import** to re-run the import without re-uploading the file.

OVAs are imported into vCenter's Templates folder as ready-to-clone VMs and are
**not** ISO install media. Use the `clone_vcenter` template source type to
build from an OVA-derived VM.

The "ISO install" picker may show previously-uploaded ISOs that are still
importing (`Importing…`, greyed-out). Wait for the import to finish or check the
image list for errors. If the picker is empty, use **Admin → Images** to upload
an ISO first.

**An unattended install finishes when the VM powers itself off.** The generated
config ends with `shutdown: poweroff`, and the worker waits for that, not for
VMware Tools. The Ubuntu Server installer runs VMware Tools *inside the
installer environment*, roughly 40 seconds after power-on and long before
anything is written to disk. After the VM powers off, the worker detaches both
CDs, boots the installed system, waits for *its* Tools, and moves the template
to `configuring`.

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

Click **Open Build Console ↗** in the wizard to open a browser-native VM
console. No vCenter login, VMware Remote Console installation, or port
forwarding is needed.

Inside the console you can:

- Type and click into the guest OS exactly like a vCenter Web Console
- **Paste from clipboard** (`Ctrl+Shift+V` or the 📋 Paste button)
- Use **Text Input** drawer if your browser blocks clipboard access
- Send **Ctrl+Alt+Del**

The wizard also has **Start / Stop / Restart / Reset** buttons for the staging
VM. Use them if the build VM hangs, needs a reboot after installing software,
or needs to be restarted. They act on the build VM only, never on a student pod.

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

> [!warning]
> **If the staging Windows VM has a vTPM, BitLocker must be fully decrypted and
> protection off before Generalize.** Crucible gives every subsequent clone a
> replacement vTPM identity, so TPM-sealed keys from the staging source are
> intentionally unavailable in publish-smoke, health-check, and student clones.
> Verify `Get-BitLockerVolume` reports `VolumeStatus: FullyDecrypted` and
> `ProtectionStatus: Off`. Crucible does not currently rekey VM configuration
> metadata during cloning, so a clone can retain the source's vCenter
> configuration key ID; that ID is not the vTPM identity.

When state flips to `ready`, the staging VM is no longer interactive —
the console button disappears.

You are not asked for the guest's username and password. Crucible uses, in
order: whatever you supply explicitly, then the template's
`default_username` / `default_password`, then — for a template built by an
**unattended ISO install** — the account the installer created, which the
platform generated and recorded when it built the seed ISO. That last case
matters because those credentials are not displayed anywhere in the wizard.

**Reaching `ready` means the cleanup provably finished**, not merely that the
VM powered off. On Linux the cleanup script deliberately leaves the guest
running so Crucible can collect an exit code and read a completion marker; the
platform then powers the VM down. If either signal is missing the template goes
to `error` rather than `ready`. See
[Generalize failed, or the template published but clones behave oddly](troubleshooting.md#generalize-failed-or-the-template-published-but-clones-behave-oddly)
for each outcome.

## Step 5 — Publish

Click **Publish to students**. The template first enters `verifying`,
where Crucible runs an automated smoke test (clone a throwaway pod →
boot it unattended → confirm it comes up → destroy it). If it passes,
the template becomes `active` and visible in the public catalog. If the
smoke test fails, the template drops back to `ready` with the failure
recorded — fix the image and Publish again. **Unpublish** an active
template at any time to hide it without losing the generalized image.

## Step 5.1 — Template Visibility (Instructor-Only Staging)

When you publish a template, you can mark it as **"Instructor only"** to
hide it from the student template picker while you stage or test it. This
is useful when:

- A template is nearly ready but still needs final tweaks or testing
- You want instructors to test a new template before students access it
- You're preparing a template for use in a future course

**In the template edit dialog:**

1. Look for the **Visibility** toggle (appears when editing or creating)
2. Choose:
   - **Public** (default) — template appears in the student template catalog
   - **Instructor only** — template is hidden from students and only
     visible to instructors and admins in the template picker

**Visibility and publish state are independent.** An active instructor-only
template is fully functional but does not appear in the student-facing catalog.
An instructor can still deploy it for testing. Students cannot see it or reach
it through another path; using its ID returns a permission error.

Once you're satisfied with the template, change it back to **Public** so
students can access it.

---

## Linux template contract

> [!caution]
> **A Linux template that violates this contract still clones and boots —
> the student just silently cannot log in.** There is no error in the
> wizard, no failed job, no alert. The pod shows "running" and green, and
> the failure surfaces only when the student tries to SSH or open the
> console and their password is refused. Get these five things right
> *before* you Generalize.

Crucible clones your template per student and injects a **unique per-pod
password** over VMware guestinfo. That injection only works if the guest
image satisfies the contract below. The publish gate validates the row-level
parts it can see: blank `default_username` / `default_password` on any
non-customized template, and blank or non-`student` `default_username` on
a customized Linux template. The guest-internal pieces are still yours to
get right inside the build console.

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

> [!caution]
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

> [!caution]
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

> [!caution] Order before `ssh.service`, **never** before `ssh.socket`.
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

On **Ubuntu 24.04** the SSH daemon unit is `ssh.service`, not `sshd.service`,
and it is socket-activated. Order host-key regeneration only
`Before=ssh.service`; this is sufficient even when the socket accepts the
connection first.

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

> [!caution]
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

**A `cloudinit_cidata` ISO build does all five of these for you.** If you
provision from an Ubuntu Server ISO with `unattend_mode=cloudinit_cidata`, the
generated autoinstall installs open-vm-tools and cloud-init, writes
`99-crucible.cfg`, installs and enables the host-key regeneration unit,
configures the apt proxy, and drops in passwordless sudo. Run the verification
block below anyway; it proves the setup.

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

> [!caution]
> **`sudo -n true` is the single most important line here.** It is the only
> one that fails the way generalize fails: no tty, no prompt, non-zero
> exit. If it does not print `sudo OK`, Generalize will abort partway and
> leave the template with a populated `/etc/machine-id` and the original
> SSH host keys — which means every clone shares them.

Also confirm the template row's **default_username is `student`** (for a
customized Linux template) or holds real static credentials (for a
non-customized template on any OS). The wizard's publish gate blocks blank
`default_username` / `default_password` on non-customized templates, and
blank or non-`student` `default_username` on customized Linux templates,
for exactly this reason.

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

If the console shows "Connecting…" indefinitely, the staging VM might not have
power yet (early `provisioning`) or might have just rebooted. Click
**Reconnect** in the console toolbar after about 30 seconds.

If Generalize fails, open the console, log in with the credentials you
provided, and check `/var/log/cloud-init.log` (Linux) or
`C:\Windows\System32\Sysprep\Panther\setupact.log` (Windows). The wizard's
**Last error** field also shows the worker report.

The console disconnects after sysprep runs because sysprep reboots the guest and
kills the WebMKS session. The wizard auto-advances to `ready` once the worker
confirms the snapshot.

See [troubleshooting.md](troubleshooting.md) for more general help.

---

## L1 Template Revalidation

Active templates in the `l1` trust tier receive a full smoke-clone validation
at least once every seven days. Revalidation is alert-only: a failed check
records the failure and alerts operators, but does not automatically unpublish
the template.

The worker checks persisted `last_validated_at` timestamps every five minutes.
It also checks immediately when an elected worker starts or a follower becomes
leader. The seven-day interval is therefore a database due-state threshold, not
a process-local timer; restarts and leader failovers cannot postpone overdue
work for another week.

Only one pending or executing `template_revalidate` job can exist per template.
Startup catch-up, a periodic poll, and leader failover may all discover the same
overdue template, but enqueue is serialized in the database and later attempts
reuse the active job.

The API repository does not own the live PrometheusRule. The observability
deployment should install these rules (the scheduler's default poll is five
minutes):

```yaml
- alert: CrucibleL1ValidationSchedulerStale
  expr: |
    absent(crucible_l1_validation_scheduler_last_success_timestamp_seconds)
    or
    (time() - crucible_l1_validation_scheduler_last_success_timestamp_seconds > 1800)
  for: 15m
  labels:
    severity: warning
  annotations:
    title: L1 validation scheduler is stale
    summary: Crucible L1 validation scheduler has not succeeded for 30 minutes
    description: Check the elected provision-worker, database connectivity, and Pushgateway delivery before L1 template validation approaches its eight-day SLA.
    runbook_url: https://github.com/jmal1/selfservice-api/blob/main/docs/instructor/templates.md#l1-template-revalidation

- alert: CrucibleL1ValidationSchedulerErrors
  expr: increase(crucible_l1_validation_scheduler_errors_total[15m]) > 0
  for: 5m
  labels:
    severity: warning
  annotations:
    title: L1 validation scheduler is reporting errors
    summary: Crucible L1 validation scheduler is failing
    description: Inspect provision-worker logs for component=l1_validation_scheduler and resolve database, enqueue, leadership, or metrics-push errors.
    runbook_url: https://github.com/jmal1/selfservice-api/blob/main/docs/instructor/templates.md#l1-template-revalidation

- alert: CrucibleL1TemplateValidationApproachingSLA
  expr: time() - crucible_template_last_validated_timestamp > 648000
  for: 15m
  labels:
    severity: warning
  annotations:
    title: L1 template validation is approaching the SLA
    summary: An L1 template has not completed validation for 7.5 days
    description: Identify the template_id series, inspect its active template_revalidate job, and resolve worker or vCenter failures before eight days.
    runbook_url: https://github.com/jmal1/selfservice-api/blob/main/docs/instructor/templates.md#l1-template-revalidation
```

Scheduler metrics also expose the latest run timestamp, due-template count,
and enqueued-job count as
`crucible_l1_validation_scheduler_last_run_timestamp_seconds`,
`crucible_l1_validation_scheduler_due_templates`, and
`crucible_l1_validation_scheduler_enqueued_jobs`.

---

## Template Health Checks

Crucible runs automated health checks for every student-visible template every
**12 hours** to catch silent rot — a template whose vCenter object was deleted,
whose disk was moved, or whose base OS no longer boots — before students hit it
during a lab session.

"Student-visible" means active, not internal, in the `active` template state,
**and** visibility `public`. A template you have marked **Instructor only** is
deliberately *not* health-checked: it is staging content no student can reach,
so alerting on it would be noise. It starts being checked as soon as you flip
it to public — you do not have to wait for the next cycle, see below.

### What gets checked

Each cycle has two layers:

| Layer | Frequency | What it does |
|-------|-----------|--------------|
| **Structural** | Every template, every 12h cycle | Verifies the vCenter VM/template object still exists in inventory. Cheap: one API call per template, no VM created. |
| **Deep** | One template per 12h cycle, rotating | Clones the template → powers it on → waits for a guest IP → destroys the clone. Proves the full boot path works end-to-end. |

Every template is deep-checked in turn, so the full rotation period is
`12h × number_of_templates`. For a lab with 10 templates, each template gets a
full deep check roughly every 5 days.

### Anti-flap: when does "unhealthy" alert?

A raw failed check never pages by itself.

- Within each cycle, each failing check is retried **3 times with exponential backoff** to absorb transient vCenter blips.
- Structural failures still require two separate 12-hour cycles.
- A first failed deep cycle persists a pending failure and schedules a durable `template_health_confirm` job after a bounded backoff (5 minutes by default, capped at 30 minutes). The job creates a new clone and is therefore an independent observation with fresh validation artifacts.
- Only that independently scheduled failure can advance deep health to the existing two-failure `unhealthy` threshold. A passing confirmation clears the pending failure immediately.
- Structural and deep counters are independent. A passing structural check can no longer erase a confirmed deep failure; a passing check clears only its own failure state.

Pending confirmation state and the job both live in PostgreSQL. Startup, periodic, and leader-failover repair paths may race safely without creating duplicate confirmation jobs.

### Checker-vs-template failures

Crucible distinguishes between "this template is broken" and "the checker itself cannot reach vCenter." A vCenter outage sets a single `crucible_template_health_checker_up=0` metric — it does **not** mark every template unhealthy. Look for the `CrucibleTemplateHealthCheckerDown` alert first; if it is firing, per-template status should be ignored until connectivity is restored.

### Viewing health status

Instructors can see per-template health at:

```
GET /api/v1/admin/templates/health
```

Each row includes:
- `health_status` — `"healthy"`, `"unhealthy"`, or `"unknown"` (not yet checked)
- `consecutive_failures` — the maximum of the structural and deep counters
- `structural_consecutive_failures`, `deep_consecutive_failures` — independent confirmation state for each check
- `last_structural_check_at` — when the last structural check ran
- `last_deep_check_at` — when the last full deep check ran
- `last_error` — the most recent error message (truncated to 256 chars)
- `last_structural_passed`, `last_deep_passed` — raw attempt outcomes
- `last_structural_error`, `last_deep_error` — full persisted diagnostics (truncated to 256 chars)
- `last_structural_fault_class`, `last_deep_fault_class` — stable diagnostic categories such as `vsphere_virtual_disk_corrupt_or_unsupported`
- `last_structural_duration_seconds`, `last_deep_duration_seconds` — raw attempt duration
- `pending_deep_failure_at`, `deep_confirmation_due_at` — the durable confirmation schedule, when present

Templates appear in this list as soon as they are active, with `health_status="unknown"` until the first check cycle completes.

### Interpreting an unhealthy template

`health_status = "unhealthy"` is persisted, confirmed state. It means either two separate structural cycles failed or a failed deep cycle was followed by a separately scheduled failed confirmation. The `last_error` and per-check diagnostic fields say what went wrong.

**Common causes:**

| `last_error` pattern | Likely cause |
|----------------------|--------------|
| `vCenter object "vm-XXXX" does not exist` | The template VM was deleted from vCenter inventory |
| `clone failed: source VM "…" not found` | Same as above (deep check also detected it) |
| `power-on failed: …` | Disk/config issue — the template VM exists but won't start |
| `wait-for-IP failed: no IP within 10m` | OS boot hangs or guest tools not installed |
| `The virtual disk is either corrupted or not a supported format` | Real vSphere storage/inventory observation. It is retained as `vsphere_virtual_disk_corrupt_or_unsupported`; inspect datastore health and vCenter task history even if a later confirmation passes. |

**What to do:**
1. Check the last error message at `GET /api/v1/admin/templates/health`.
2. Open vCenter and verify the template VM exists at the expected path.
3. If the VM is missing, re-publish the template through the wizard.
4. If the VM exists but won't boot, attach a console and investigate the OS.

Once the underlying issue is fixed, the relevant passing check clears its own failure state automatically. No manual reset is needed.

### Prometheus and Grafana contract

The worker sends a complete PostgreSQL-backed snapshot with Pushgateway **PUT/replacement semantics**. Deleted templates, obsolete label sets, and raw attempts that are no longer in persisted state disappear on the next snapshot, including the startup/failover repair snapshot. The pusher has no process-lifetime result map.

| Metric | Contract |
|--------|----------|
| `crucible_template_health_status{template}` | Confirmed persisted state only: `1` healthy, `0` unhealthy, `-1` unknown. Never derived directly from a raw attempt. |
| `crucible_template_health_consecutive_failures{template}` | Maximum persisted structural/deep policy counter. |
| `crucible_template_health_attempt_status{template,check_type}` | Last raw structural/deep observation: `1` pass, `0` fail. Diagnostic only. |
| `crucible_template_health_last_check_timestamp_seconds{template,check_type}` | Persisted timestamp for that raw observation. |
| `crucible_template_health_last_check_duration_seconds{template,check_type}` | Duration of that raw observation. |
| `crucible_template_health_fault_info{template,check_type,fault_class}` | Classified last failure. Full text remains in PostgreSQL and worker logs. |
| `crucible_template_health_deep_confirmation_pending{template}` | `1` while the first deep failure awaits independent confirmation. |
| `crucible_template_health_deep_confirmation_due_timestamp_seconds{template}` | Durable job eligibility time. |
| `crucible_template_health_checker_up` | `1` after the latest full reconciliation or startup replacement probe reached vCenter; `0` for a checker-level vCenter failure. Startup replacement always emits the series. |
| `crucible_template_health_checker_last_success_timestamp_seconds` | Conservative persisted freshness timestamp: every currently visible template has a structural result at least this new. |
| `crucible_template_health_snapshot_timestamp_seconds` | Complete snapshot generation time; useful for Pushgateway delivery diagnostics, not confirmation evidence. |

This repository does not own the live Grafana resources. The proposed `CrucibleTemplateHealthFailing` expression is:

```promql
(crucible_template_health_status == 0)
and on() (crucible_template_health_checker_up == 1)
and on()
  (time() - crucible_template_health_checker_last_success_timestamp_seconds < 30 * 60 * 60)
```

Use `for: 15m` only for scrape/deploy stability, not as a substitute for confirmation. Proposed annotations:

```yaml
summary: 'Confirmed template health failure: {{ $labels.template }}'
description: 'PostgreSQL marks {{ $labels.template }} unhealthy after the required independent confirmation. The checker is fresh; inspect per-check fault metrics, the admin health endpoint, and vCenter task history.'
runbook_url: 'https://github.com/jmal1/selfservice-api/blob/main/docs/instructor/templates.md#prometheus-and-grafana-contract'
```

Raw failure panels may query `crucible_template_health_attempt_status == 0`, but that expression must not page. Proposed checker freshness expression:

```promql
absent(crucible_template_health_checker_last_success_timestamp_seconds)
or
(time() - crucible_template_health_checker_last_success_timestamp_seconds > 30 * 60 * 60)
```

The proposed `CrucibleTemplateHealthCheckerDown` expression remains `crucible_template_health_checker_up == 0` with `for: 5m`.

Keep the currently paused live template-health rule paused through deployment. After migration and worker rollout, verify the Pushgateway group contains no legacy `crucible_template_health_status{check_type=...}` series, compare the confirmed status metric with `GET /api/v1/admin/templates/health`, and observe one full snapshot. Only then replace the live expression and unpause the rule.

### Orphan cleanup

The deep check names each clone `crucible-healthcheck-<template-uuid>-<attempt-id>`. If the worker crashes mid-check, the clone may be left behind on the datastore. Crucible sweeps for old orphaned clones whenever a worker becomes the elected leader, but retains recent clones so failover cannot destroy an active confirmation claimed by another worker. The distinctive prefix ensures the sweep cannot match a student pod VM.

### When cycles actually run

The 12-hour timer lives in the worker process, and Crucible is deployed several
times a day, so relying on that timer alone would mean a cycle never completed.
Instead, whenever a worker becomes the elected leader — at startup, after a
deploy, or after a failover — it checks when the last cycle finished:

- **More than 12 hours ago (or never):** it runs a cycle immediately. A freshly
  deployed environment therefore has health data within minutes, not 12 hours.
- **Less than 12 hours ago:** it skips. This is why a burst of deploys does not
  produce a burst of deep checks — each deep check clones a real VM, and doing
  that on every deploy would put avoidable load on the datastores.

The practical consequence: after you publish a template or flip one to public,
health data appears on the next cycle boundary, or immediately if a deploy or
failover happens to land while a cycle is already due.

---

See [troubleshooting.md](troubleshooting.md) for more general help.
## Pinning templates and blueprints

Templates and blueprints can be **pinned** to emphasize them. Pinned items appear in a dedicated **Pinned** section at the top of the template and blueprint picker, making them immediately visible to students without scrolling through a long list.

### How pinning works

- **Multiple pins allowed** — Pin as many templates or blueprints as you like.
- **Ordered** — Pinned items appear in the order you set; drag them to reorder (or use up/down buttons on mobile).
- **Still in main list** — Pinned items also stay in the normal alphabetical list below the Pinned section. Pinning is emphasis, not filtering.
- **Instructor/admin only** — Only instructors (role ≥ instructor) can pin or unpin. Students see the Pinned section read-only.

### When to pin

Pin this week's material so students land on it immediately:

- The lab template for the current module
- The starter blueprint for an active project
- A frequently-used tool or reference template

### Viewing and managing pins

In **Admin → Templates** or **Admin → Blueprints**, you'll see a **Pin** icon (📌) next to each item. Click it to pin; click again to unpin. Pinned items show a special **Pinned** badge, and you can drag them to reorder (or use ↑/↓ buttons). The order you set here is what students see in the Pinned section at the top of their picker.

---

## Related pages

- [overview.md](overview.md) — How templates fit into pods, blueprints, playlists
- [glossary.md](glossary.md) — Definitions of `template`, `blueprint`, `pod`, etc.
- [troubleshooting.md](troubleshooting.md) — Common provisioning failures
