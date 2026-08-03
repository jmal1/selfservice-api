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

## "Action status is `error`, message says a command is not available" (exit 127)

A tool your workflow script calls is not installed in the assessment runner.

| What the student sees | What the instructor sees |
|---|---|
| "This check could not run: the command `X` is not available in the assessment runner." | `status: error`, `exit_code: 127`, message names the missing command |

The runner classifies exit 127 as `error` rather than `fail` to distinguish an infrastructure defect from a student mistake. The student's result panel says the problem is not their fault and asks them to report it.

> [!warning]
> The **workflow editor catches this at authoring time** (warning **CRU0002**) when you save a
> script that calls a command not in the runner image. If you saw CRU0002 and dismissed it, that
> is the same root cause surfacing at runtime.

**Immediate fix:**

1. Check the [Pre-installed tools table](runner-environment.md#pre-installed-tools) — does the
   command you need appear there? That table is the authoritative inventory and is guarded by a
   test, so it cannot drift from the actual image.
2. If it is not listed, either:
   - Switch to an equivalent tool that **is** in the image.
   - Ask a platform admin to add it to the runner tool manifest and rebuild the runner image.
3. Do **not** `apt install` from the script — the runner has no internet egress, and
   mutating a shared image mid-run would affect other running assessments.

| Tools **not** in the image (common requests) |
|---|
| `gobuster`, `sqlmap`, `wfuzz`, `hashcat`, `tcpdump`, `mtr`, `iperf3`, `httpie`, `yq`, `xmlstarlet` |

If the message reads *"a tool it depends on is not available"* (without naming it), the missing
binary is inside a **library action** body. Trace which library action your script calls via
`run_action`, then check that action's bash body against the pre-installed tools table.

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

## "Generalize failed, or the template published but clones behave oddly"

Generalize runs its cleanup through VMware guest ops, which gives it **no
tty**. If the build user cannot `sudo` without a password, the script
aborts partway — and what it had already done still stands. Symptoms of a
half-run generalize:

| Symptom | What didn't run |
|---|---|
| Two clones get the **same DHCP lease**, or one steals the other's IP | `truncate -s 0 /etc/machine-id` |
| `ssh` warns **REMOTE HOST IDENTIFICATION HAS CHANGED** between two pods, or accepts the same host key for both | `rm -f /etc/ssh/ssh_host_*` |
| A clone's first boot doesn't apply the injected password | `cloud-init clean` |

Check it in one line from the build console:

```bash
sudo -n true && echo "sudo OK"
```

If that does not print `sudo OK`, fix requirement 5 in the
[Linux template contract](templates.md#linux-template-contract), then
re-run Generalize. Being in the `sudo` group is **not** sufficient —
Ubuntu's stock rule still demands a password.

> [!warning]
> These are the two failures that are **invisible on one clone**. A single
> pod from a badly-generalized template looks perfect. Test with two pods
> from the same template and compare `cat /etc/machine-id` and
> `ssh-keyscan <ip>` — the values must differ.

### How Crucible knows generalize actually finished

Generalize's last act is to power the guest off, which kills the VMware Tools
agent *while the platform is still talking to it*. So the guest-ops call
**always** returns an error — even when everything worked. The message vCenter
returns is:

```
ServerFaultCode: The guest operations agent could not be contacted.
```

That exact message is also what you get when VMware Tools **never started at
all**, so it cannot be used to tell success from failure.

Instead, the Linux generalize script records its own completion. Its final
command before `shutdown` stamps the job's ID into a `guestinfo` variable:

```bash
vmware-rpctool "info-set guestinfo.crucible.generalize.job <job-id>"
```

`guestinfo` lives in the VM's configuration rather than in the guest, so it
survives the power-off and the platform reads it back afterwards. Because the
value is the **job ID** and not a fixed word, a sentinel left over from an
earlier generalize attempt on the same VM cannot be mistaken for this one.

What that means for you:

| Template state after Generalize | What it tells you |
|---|---|
| `ready` | The cleanup script ran to its final line. Trustworthy. |
| `error`, *"generalize script did not run to completion"* | The guest went down **before** cleanup finished — usually the `sudo` problem above. Fix it and re-run Generalize; do not publish. |
| `error`, *"completion could not be verified"* | vCenter was unreachable when the platform tried to confirm. The template may be fine; re-run Generalize to get a clean answer. |

> [!note]
> Windows is exempt from the sentinel. Sysprep is launched fire-and-forget and
> powers the machine off on its own schedule, so there is no opportunity to
> stamp anything. Windows generalize still relies on the shutdown signal.

---

## "apt is broken on my template or on every clone"

If `apt-get update` fails on a template you built from an ISO, look at the
proxy the installer wrote:

```bash
cat /etc/apt/apt.conf.d/90curtin-aptproxy
```

It must contain a bare URL:

```
Acquire::http::Proxy "http://10.10.30.20:3142";
```

Anything else — a Python-style dict, a stray `{`, an empty string — means
apt has a syntactically valid config pointing at a nonexistent proxy, so
**every** fetch fails while the file itself parses fine. Delete the file
(or correct it to the line above) and re-run `apt-get update`. Report it
if a freshly-provisioned template shows this: the generator writes that
file, so it is a platform bug rather than something you did.

---

## "SSH is refused on my Linux template (or on every clone of it)"

Symptom: the VM boots, VMware Tools report it as running, it holds a DHCP
lease and you can reach it — but connecting to port 22 gives **connection
refused** rather than a timeout. `systemctl is-active ssh` reports
`inactive`, which is *normal* on Ubuntu 24.04 (SSH is socket-activated), so
that alone is not the answer. Check the socket instead:

```bash
systemctl is-active ssh.socket        # want: active
systemctl is-enabled ssh.socket       # want: enabled
sudo ss -lnt | grep ':22'             # want: a LISTEN line
```

The revealing case is `enabled` **but** `inactive`, with no journal entries
for the unit at all (`journalctl -u ssh.socket` prints `-- No entries --`).
That means systemd never even tried to start it. Confirm with:

```bash
journalctl -b | grep -i 'ordering cycle'
```

If you see this, you have a **systemd ordering cycle**:

```
sockets.target: Found ordering cycle on ssh.socket/start
sockets.target: Job ssh.socket/start deleted to break ordering cycle
```

systemd resolves a cycle by *deleting one of the jobs in it*, and here it
deleted the job that binds port 22. Nothing errors, nothing is marked
failed, and `systemctl list-units --failed` is empty — the port simply never
opens.

The usual cause is a custom unit that declares `Before=ssh.socket` while
being pulled in by `multi-user.target` (which is ordered *after*
`sockets.target`, which is ordered after `ssh.socket`). Crucible's own
`crucible-regen-ssh-hostkeys.service` shipped with exactly that mistake once.
The fix is to order before `ssh.service` and **not** `ssh.socket` — see the
danger callout in [Templates](templates.md). A `ConditionPathExists` guard on
the offending unit does not help: systemd resolves ordering cycles *before*
it evaluates conditions, so a unit that gets skipped anyway can still take
SSH down.

Report it if a freshly-provisioned template shows this — the generator writes
that unit, so it is a platform bug rather than something you did.

---

## See also

- [Building Workflows](workflows.md) — the basics
- [Runner Environment](runner-environment.md) — what's available at runtime
- [Glossary](glossary.md) — quick term lookup
