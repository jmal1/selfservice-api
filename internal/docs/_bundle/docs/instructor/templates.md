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
   draft ──Provision──▶ provisioning ──auto──▶ configuring ──Generalize──▶ generalizing ──auto──▶ ready ──Publish──▶ active
                                                  ▲                                                    │
                                                  └────────────── Reconfigure ─────────── Unpublish ◀──┘
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
| `active` | Published — students can launch pods from it | No |
| `error` | A worker job failed; check Last error in the wizard | No |

---

## Step 1 — Draft

Go to **Admin → Templates → New**. Fill in:

- **Name** — human-friendly, will be shown to students
- **OS type** — `ubuntu`, `windows`, `kali`, etc. (drives the generalize behavior)
- **Source** — clone from an existing Crucible template, or paste a vCenter VM moref
- **Staging network** — defaults to `LabVMs-VLAN30`; only change if you know why

Click **Create draft**. You'll land on the wizard page for the new template.

## Step 2 — Provision

Click **Provision**. The worker clones the source VM into the
Templates folder, attaches a NIC on the staging network, powers it on,
and waits for VMware Tools. This is the slow step.

As soon as state flips to `configuring`, the **Open Build Console**
button appears.

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

## Step 5 — Publish

Click **Publish to students**. The template becomes visible in the
public catalog. **Unpublish** at any time to hide it without losing the
generalized image.

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
