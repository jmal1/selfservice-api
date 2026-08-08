# ISO-build hardware gaps for Windows templates

> [!note]
> **Audience:** Crucible maintainers and lab admins. This is an engineering
> design note, **not** an instructor wiki page — it is deliberately kept out of
> the `make wiki-bundle` closure (referenced only by plain path, never a
> markdown link, from
> [`docs/instructor/os-recipes.md`](../instructor/os-recipes.md)).
>
> **Scope:** two real limitations of the blank VM shell Crucible creates for
> `source_type=iso` templates, both of which affect Windows builds. Each gap has
> a works-now doc-only mitigation and a cleaner code follow-up. Recommendations
> are called out; code is left as a spec + TODO where the change isn't trivially
> safe.

## Background: what `CreateBlankVM` builds today

`internal/vcenter/template_ops.go` (`CreateBlankVM` → `blankVMDevices`) creates
the installer shell for every ISO-source template. The shell is:

- **pvscsi** SCSI controller with a single thin, persistent system disk
- **EFI** firmware (`GuestOsDescriptorFirmwareTypeEfi` default)
- an **IDE** controller carrying one or two CD-ROMs (installer ISO + optional
  seed ISO)
- a **VMXNET3** NIC
- boot order: CD-ROM before disk

Critically, it adds **no Virtual TPM (vTPM)** and **does not enable Secure
Boot**. There is no key-provider wiring on this path and no `needs_vtpm` flag on
the template model. Both gaps below flow from that.

---

## Gap A — no vTPM / Secure Boot on the blank shell

### Problem

Windows 11 Setup hard-blocks in the **windowsPE** phase unless it detects TPM
2.0 **and** Secure Boot (plus a RAM floor). Because `blankVMDevices` adds
neither, an unmodified Windows 11 `windows_autounattend` build stops at
*"This PC can't run Windows 11"* and never partitions the disk. Windows 10 and
all Server SKUs covered in the recipes are unaffected — they install fine on the
vTPM-less EFI shell.

### Option (i) — autounattend LabConfig bypass *(recommended — **DONE**, shipped in #121)*

Add a `windowsPE`-pass `RunSynchronous` block to `autounattend.xml` that writes
the `HKLM\SYSTEM\Setup\LabConfig` bypass values before Setup touches the disk:

- `BypassTPMCheck = 1`
- `BypassSecureBootCheck = 1`
- `BypassRAMCheck = 1`
- `BypassStorageCheck = 1` (skips the 64 GB floor)
- `BypassCPUCheck = 1` (skips the supported-CPU allow-list)
- `HKLM\SYSTEM\Setup\BypassNRO = 1` (lets OOBE finish with a local account, no
  network / Microsoft account)

These five `Bypass*Check` values are the authoritative set (verified on 23H2 and
24H2); `BypassNRO` is an additional local-account convenience.

The full XML snippet is in the Windows 11 recipe
([`docs/instructor/os-recipes.md`](../instructor/os-recipes.md), recipe 6).

- **Pros:** zero infrastructure change; no Key Provider, no host crypto config,
  no new template field. Works on the exact shell we build today.
- **Status — handled automatically (ref #121).** Crucible's generator
  (`internal/unattend/windows.go` + `internal/provisioner/assets/windows-unattend.xml`)
  now emits the `windowsPE` LabConfig block **automatically** whenever the build
  targets Windows 11, so a Win11 build no longer needs the block added out of
  band. Win10 and Server answer files are unchanged. It remains a functional
  install-time bypass, not a security posture we'd ship to production Windows;
  for a throwaway lab image that's acceptable.

**DONE (shipped in #121):** the Windows answer-file generator now appends the
`windowsPE` LabConfig block **only** when the target is Windows 11. Gating
matters: the block is unnecessary on Win10/Server and adding a `windowsPE`
`Microsoft-Windows-Setup` component unconditionally would change every Windows
build's answer file (and its golden test vector in
`internal/unattend/unattend_test.go`). The implemented wiring: the caller
(`internal/provisioner/template_jobs.go`) sets a `BypassWin11HardwareChecks`
bool on the unattend `Spec` when `payload.GuestID == "windows11_64Guest"`, and
`internal/unattend/windows.go` branches on it in the answer-file template. The
`{{if}}` whitespace is crafted so the flag-off answer file is byte-identical to
today, so only Win11 vectors change. Tests assert the five ordered LabConfig
keys appear before `specialize` for Win11 and are absent (identical header)
otherwise.

### Option (ii) — add optional vTPM + Secure Boot to the blank shell *(cleaner, follow-up)*

Give `CreateBlankVM` the ability to attach a `VirtualTPM` device and set
`bootOptions`/firmware for **Secure Boot** when the guest is
`windows11_64Guest` or when a new `needs_vtpm` template flag is set. (Note the
Guest OS ID trap: Win10 is `windows9_64Guest`, but **Win11 is
`windows11_64Guest`** — they are distinct, so a vTPM gate keyed on the guest ID
must target `windows11_64Guest`.)

- **Requires:** a vSphere **Key Provider** (native key provider or a KMS) so the
  vTPM has somewhere to seal keys, and the service account needs
  **Cryptographer** privileges (e.g. *Cryptographic operations → Manage keys /
  Encrypt / Clone*). This mirrors the encryption/crypto wiring already needed on
  the clone path for customized VMs — reuse that pattern rather than inventing a
  new one.
- **Pros:** Windows 11 installs *honestly* (real TPM + Secure Boot), no registry
  bypass, closer to a real machine, and future Windows versions that tighten the
  check keep working.
- **Cons:** infra dependency (Key Provider must exist and be healthy on every
  target host/cluster), broader permissions, and a new device on the shell that
  the generalize/clone paths must tolerate. Higher blast radius than a doc-only
  answer-file tweak.

**Recommendation:** **(i) is shipped** (#121 — the gated generator change is
live, plus this doc); pursue **(ii)** as the durable fix once a Key Provider is
provisioned and we're ready to require it. Cite: `blankVMDevices` /
`CreateBlankVM` in `internal/vcenter/template_ops.go`.

---

## Gap B — Windows Setup may not see the pvscsi disk (all Windows SKUs)

### Problem

The shell's system disk hangs off a **pvscsi** controller. **No** Windows Setup
media — Windows 10, Windows 11, or Server 2016/2019/2022/2025 — ships an in-box
pvscsi driver (VMware KB 1010398), so any Windows ISO build can stop at
*"We couldn't find any drives. To get a storage driver, click Load driver."* and
be unable to proceed. In this lab's field history the failure has been reported
most on Server builds, but the root cause is common to every Windows installer,
so every Windows recipe must account for it.

**Constraint that shapes the fix:** `blankVMDevices` builds a **single IDE
controller with at most two CD-ROMs** — CD-ROM 0 = installer ISO, CD-ROM 1 =
the seed ISO (`SeedISOPath`, set in `internal/provisioner/template_jobs.go`). The
seed ISO the pipeline generates (`internal/unattend/windows.go` →
`buildAutounattendISO`) contains **only** `autounattend.xml`; there is no third
slot and no VMware Tools media mounted. So neither a `windowsPE` driver-injection
that reads the Tools ISO **nor** the manual **Load driver** fallback has anything
to point at today without a pipeline change. This is why LSI SAS (Option 2) is
the pragmatic fix — now shipped in #122 for Server guest IDs.

### Option 1 — slipstream the VMware pvscsi driver into the seed media

Stage the correct VMware Tools `pvscsi` driver onto the **seed ISO** alongside
`autounattend.xml`, then reference it from a `windowsPE`-pass
`Microsoft-Windows-PnpCustomizationsWinPE` component (`<DriverPaths>` →
`<PathAndCredentials>`) so WinPE loads it before disk selection. Driver folder by
OS (on the VMware Tools installer ISO layout):

- **Win11 / Server 2022 / Server 2025** (Tools ≥ 12): `…\pvscsi\Win10\amd64`
- **Win10 / Server 2016 / Server 2019** (Tools ≥ 11.2): `…\pvscsi\Win8\amd64`

- **Pros:** keeps the high-performance pvscsi controller for every guest;
  uniform shell across OSes; fixes client Windows too.
- **Cons:** the seed-build path must copy the correct signed pvscsi driver onto
  the seed ISO **and** the generator must emit a `windowsPE`
  `PnpCustomizationsWinPE` block — neither exists today (the generator emits only
  `specialize` + `oobeSystem`). More moving parts and driver/OS-version drift to
  maintain. The manual **Load driver** fallback also only works once some Tools
  media is mounted, which likewise needs a pipeline change.

### Option 2 — use LSI SAS for Server guest IDs *(recommended — **DONE**, shipped in #122)*

For Server guest IDs, build the shell with an **LSI SAS** controller instead of
pvscsi. LSI SAS has an in-box Windows driver, so Setup sees the disk with no
slipstreaming and no extra mounted media.

- **Field history:** LSI SAS is the controller that has historically worked for
  WS2022/WS2025 ISO builds in this lab; pvscsi is where the "no drives" reports
  came from. That real-world signal favors LSI SAS for Server.
- **Pros:** no driver staging, no per-version driver maintenance, no need for a
  third CD slot, matches what already works. The performance delta is irrelevant
  for a teaching template.
- **Cons:** a second controller code path in `blankVMDevices` (branch on guest
  ID family), and mildly lower theoretical throughput than pvscsi. Client Windows
  10/11 on pvscsi still needs Option 1 (or an LSI SAS shell of its own) to be
  fully hands-off.

**DONE (shipped in #122):** `blankVMDevices` now branches to
`CreateSCSIController("lsilogic-sas")` when `p.GuestID` is a Windows **Server**
identifier — the `isWindowsServerGuestID` helper matches `windows9Server64Guest`
(WS2016), `windows2019srv_64Guest` (WS2019), `windows2019srvNext_64Guest`
(WS2022), and `windows2022srvNext_64Guest` (WS2025) — keeping pvscsi for Linux
and for **client** Windows (`windows9_64Guest`, `windows11_64Guest`). A vcsim
unit test asserts Server IDs → one LSI SAS controller with the disk attached and
non-Server IDs → pvscsi. **Client** Windows 10/11 therefore still build on pvscsi
and keep the manual **Load driver** fallback until a future **Option 1**
(seed-staged pvscsi driver) lands. Cite: `blankVMDevices` in
`internal/vcenter/template_ops.go`.

**Status:** landed in #122 as a focused, separately-tested change —
`internal/vcenter/template_ops.go` `blankVMDevices` guest-ID branch +
`isWindowsServerGuestID` helper + a vcsim test asserting the controller type per
guest-ID family. A future **Option 1** would extend hands-off disk visibility to
client Windows (Win10/Win11), which stay on pvscsi today.

---

## Summary

| Gap | Works-now mitigation | Durable fix | Recommended path |
|-----|----------------------|-------------|------------------|
| **A** — no vTPM / Secure Boot (blocks Win11) | LabConfig registry bypass in `autounattend.xml` `windowsPE` pass — **DONE (#121):** the generator now emits it automatically, gated to `windows11_64Guest` | Optional vTPM + Secure Boot on the blank shell via a Key Provider + Cryptographer perms | **(i) shipped (#121)**, **(ii)** later |
| **B** — pvscsi disk invisible to Windows Setup (all SKUs; Server hit hardest in field) | LSI SAS controller for Server guest IDs — **DONE (#122):** `blankVMDevices` builds Server shells on LSI SAS automatically; client Win10/11 keep the manual Load-driver fallback | Option 1 (seed-staged pvscsi driver) to make client Windows hands-off too | **Option 2 shipped (#122)**; Option 1 later |

Both fixes touch `internal/vcenter/template_ops.go` (`CreateBlankVM` /
`blankVMDevices`); Gap A option (i) additionally touches the Windows answer-file
generator (`internal/unattend/windows.go`,
`internal/provisioner/assets/windows-unattend.xml`). Neither code change is
included in the docs PR that introduced this note — they are specced with
explicit TODOs so they can land as small, separately-tested changes.
