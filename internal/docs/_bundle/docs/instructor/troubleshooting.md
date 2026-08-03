# Troubleshooting

Workflows fail. Sometimes for good reasons (the student didn't do the
work), sometimes because the script is wrong. This page is a
field guide for the second case.

---

## "I get 403 Forbidden from an admin page or endpoint"

Members of the **`lab-instructors`** group have the full admin panel:
Overview, Users, Templates, Blueprints, Actions, Workflows, Playlists,
Runs, VLAN Pool, Jobs, and Health. If you get a `403` on any of those,
something is wrong — check that your account is actually in `lab-instructors`
(sign out and back in to refresh your session's group claim), then report it.

The **one** place a `403` is expected and correct is the **Audit Log**
(`/admin/audit`) and the active-Sessions view — those stay admin-only. Your
own actions are still written to the audit log; you just can't read it back.
If you need an audit trail, ask a platform admin.

---

## "My workflow always passes — even on a fresh, untouched pod"

This is the worst kind of bug because students think they've succeeded.
Usual causes:

| Cause | Fix |
|---|---|
| Forgot `set -e` (or `set -euo pipefail`) | Add it at the top of the script |
| Used `\|\| true` somewhere — failures get swallowed | Remove the `\|\| true`, or replace with explicit error handling |
| `grep` returned nothing but you didn't check the exit code | Use `grep -q ...` and check `$?`, or `if ! grep -q ...; then exit 1; fi` |
| SSH command failed but you only checked the local exit code | The `ssh` exit code IS the remote exit code — check it explicitly |
| Wrong execution mode (e.g. ran `kali_runner` check meant for `vmware_tools`) | See [Runner Environment](runner-environment.md) |

> [!caution]
> Always test against a known-bad pod. A workflow you haven't seen fail
> at least once is not a workflow you can trust.

---

## "My workflow always fails — even on a known-good pod"

The script is broken or the environment is wrong. Walk through:

1. **Read the instructor output.** Click the failed workflow in the
   results panel — instructor-only stdout/stderr is captured there.
2. **Check the env vars.** Add `env | grep CRUCIBLE_` at the top of
   your script and run it once. Confirm `CRUCIBLE_TARGET_IP` etc. look
   right.
3. **SSH manually.** From the runner pod, can you actually reach the
   target? `ssh -o ConnectTimeout=5 "$CRUCIBLE_TARGET_USERNAME@$CRUCIBLE_TARGET_IP" 'true'`.
4. **Check the workflow status.** Edits to an `active` workflow create
   a new revision — the run might be using the version you launched
   before your fix.

---

## "Times out at 300 seconds"

Default `timeout_seconds` is 300. Either you're doing too much in one
workflow, or something is hanging. Diagnostic:

```bash
# At the top of the script, dump every command as it runs:
set -x

# Or wrap suspect blocks:
{ time some_slow_command; } 2>&1
```

Common hangs:

| Hang | Fix |
|---|---|
| SSH to a powered-off target | Add `-o ConnectTimeout=5` |
| `nc -z` against a filtered port | Add `-w 3` |
| `nmap` of a /24 | Don't. Scope it. |
| `apt update` on the runner | Won't work anyway — no internet |
| Waiting on `read` because of an interactive prompt | `< /dev/null` or use `-y` / `--yes` flags |

> [!tip]
> If a workflow genuinely needs more than 60 seconds, **split it**
> into two workflows in the playlist. Smaller scripts = faster
> feedback for the student.

---

## "command not found: run_action"

You forgot the first line:

```bash
source /opt/crucible/lib/actions.sh
```

This must be the first non-comment line of every workflow script.

---

## "Action `foo-bar` not found"

Either the slug is mistyped, the action is in a different deployment,
or the action was deleted/renamed.

```bash
# Cross-reference: list registered actions
curl -s "$CRUCIBLE_API/api/v1/admin/actions" | jq '.[].slug'
```

> [!warning]
> Renaming an action breaks every workflow that calls it. If you've
> recently changed an action's slug, audit your active workflows.

---

## "Action refused — platform not supported"

The action's `supported_platforms` list doesn't include the target's
detected platform. Either:

- Widen the action's `supported_platforms` (e.g. add `linux:debian`)
- Use a different action that supports this platform
- Use inline bash for this case

See [Building Actions](actions.md) §"Platform tags".

---

## "Student sees no error message when they fail"

You only printed instructor output. Add a `STUDENT_MSG:` line:

```bash
if ! check_thing; then
    echo "STUDENT_MSG: Service X isn't running. Start it with: sudo systemctl start X"
    exit 1
fi
```

Or, for library actions, set `student_fail_hint` once and it applies
everywhere the action is used.

---

## "Workflow passes in `draft`, fails after `active`"

Most often: you've activated an **old revision**. When you edit an
active workflow, a new revision is created and becomes the "head", but
in-flight playlist runs continue against their launched version.

Check the Workflows admin page → revision selector. Confirm the
revision marked "head" is the one you intended.

---

## "Playlist won't promote to `active`"

The API refuses if any referenced workflow isn't `active`. Check each
workflow's status:

```bash
curl -s "$CRUCIBLE_API/api/v1/admin/playlists/{slug}" | jq '.workflows[] | {slug, status}'
```

Activate any `draft` or `approved` workflows first, then retry.

---

## "Student can't see my new playlist"

Three things to check, in order:

1. Is the playlist `status: active`?
2. Is `visible_to_students: true`?
3. Does the student have a deployed pod from the playlist's
   `blueprint_slug`? Playlists only appear to students whose pod
   matches.

---

## "It worked yesterday and now it doesn't"

| Likely cause | What changed |
|---|---|
| Runner image updated | A pre-installed tool was removed/renamed; check release notes |
| Target template updated | The default user or hostname might have changed; re-check env vars |
| Target reset to snapshot | If your workflow assumed state from a prior workflow, that's gone |
| Engine version bumped | Rare, but possible; check the engine changelog |

---

## `runner_smoke` synthetic check is firing

`runner_smoke` exercises the full Epic D path: engine dispatch → Kubernetes
Job scheduling → Multus NAD attachment → macvlan DHCP lease → Kali image
pull → action execution → callback → results persisted. When it fires, one
of those links is broken.

**First three things to check:**

1. **Is the Kali runner Kubernetes Job scheduling?**
   Check that the `crucible-engine` is dispatching runner Jobs and that the
   Job lands on node `k3sv03`. A common cause is a recent change to the
   Multus NetworkAttachmentDefinition (NAD) or the macvlan interface config.
   Look at `kubectl -n crucible get jobs` for stuck or missing jobs and
   `kubectl -n crucible describe job <name>` for scheduling errors.

2. **Did the runner pull the Kali image successfully?**
   A new node or an image tag change can cause a pull timeout that looks like
   a stall rather than an error. Check `kubectl -n crucible get pods` for
   `ImagePullBackOff` or `ErrImagePull`. The runner pod name matches the Job
   name. The image is pinned in the engine Helm values under
   `runner.image.tag`.

3. **Did the callback reach the engine?**
   The runner calls back to the engine API after each workflow completes. If
   the run reached `running` state but never advanced to `completed` the
   callback path is broken — check the engine logs for `run.callback` errors
   and confirm the runner pod can reach the engine ClusterIP. A `completed`
   run with **zero results** means the runner Job exited 0 but never called
   back at all; this is the failure mode the `zero results` assertion was
   specifically added to catch.

> [!warning]
> The `runner_smoke` check creates a real pod and dispatches a real Kali
> runner Job. When investigating a failure, check that the synthetic pod
> (`synthetic-noop-*`) was destroyed; if the check timed out mid-run the
> deferred cleanup fires but may also fail. The daily `synthetic_janitor`
> CronJob sweeps any leaked `synthetic-noop-*` pods, but you can trigger
> it manually if quota pressure is urgent.

---

If you've checked the above and the workflow still misbehaves, capture:

1. The workflow JSON (`GET /api/v1/admin/workflows/{slug}`).
2. The failing run's full instructor output.
3. The output of `env | grep CRUCIBLE_` from a successful test run.
4. Whether it ever worked, and what changed.

Then escalate to the platform admin. With those four things, root
cause usually falls out in minutes.

---

## "My ISO template sat in `provisioning` for an hour, then errored"

An unattended ISO install that never finishes almost always means the
installer is waiting for input. Open the Build Console and look at the
screen — the error message in the wizard says the same thing, but the
console tells you *which* prompt.

**`Continue with autoinstall? (yes|no)`**
: The Ubuntu installer's safeguard before it wipes the disk. Crucible
  normally answers this automatically from inside the installer
  environment, so seeing it means that automation failed. Type `yes` to
  unblock this build, then report it — every future build from that ISO
  will stall identically until it's fixed.

**A partition/disk prompt, or a language selection**
: The seed CD wasn't read at all. Check that the template's
  **unattended mode** matches the ISO: `cloudinit_cidata` only works with
  the Ubuntu **Server** installer, not the Desktop ISO (different
  installer entirely). A Desktop ISO must use `manual`.

**A blank or frozen screen**
: The VM booted from the wrong device. Re-run Provision; if it recurs,
  the ISO itself may be corrupt — re-upload it.

> [!note]
> **VMware Tools showing "running" during an ISO install means nothing.**
> The Ubuntu Server installer runs Tools inside its own live environment
> about 40 seconds after power-on, with a completely empty disk. Crucible
> deliberately ignores it and waits for the VM to **power itself off**,
> which the generated config does when the install genuinely completes.
> Don't use Tools status to judge whether an install is progressing.

## "My ISO template reached `configuring` but the disk is empty"

This shouldn't be possible any more, but if you see it: the template's
staging VM booted to a "no operating system" / PXE screen. Set the
template back to `error`, delete the staging VM, and re-provision. Report
it — it means the install-complete signal misfired, which is a platform
bug, not a mistake on your end.

---

## See also

- [Building Workflows](workflows.md) — the basics
- [Runner Environment](runner-environment.md) — what's available at runtime
- [Glossary](glossary.md) — quick term lookup
