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

## "Provisioning is temporarily unavailable for maintenance"

Crucible can intentionally pause **new** pod and VM provisioning while
operators perform infrastructure maintenance. During that window, these
requests return `503 Service Unavailable`:

- `POST /api/v1/pods`
- `POST /api/v1/blueprints/{blueprintID}/deploy`
- `POST /api/v1/pods/{podID}/vms`

The response uses the normal JSON error envelope and includes
`Retry-After: 300`:

```json
{
  "error": "Provisioning is temporarily unavailable for maintenance.",
  "request_id": "..."
}
```

This is a platform-wide maintenance state, not a problem with your template,
blueprint, quota, or request body. Wait for the maintenance window to end
before retrying. Deleting pods or VMs, power operations, and platform cleanup
remain available so existing environments can be made safe. The worker also
continues compensation-only retries for cleanup that began before maintenance;
those retries cannot resume pod or VM creation.

Operators can distinguish ordinary queued work from recovery work in the job
payload: only `pod_create` or `vm_add` jobs carrying `cleanup_only: true` remain
claimable while provisioning claims are disabled. Clone cleanup uses the exact
vCenter MoRef persisted for that attempt, never a VM display name. Failed
cleanup remains pending with capped backoff independently of the original
provisioning retry limit. Successful compensation records the parent job as
failed with `compensated: true`; ambiguous ownership records
`manual_cleanup_required: true` and requires operator resolution rather than
deleting an uncertain VM.

A transient database failure while rescheduling compensation does not strand
the job on a live worker. The worker retries the durable pending-state write
with bounded backoff until it succeeds. On worker shutdown, startup recovery on
the replacement process performs the handoff; Crucible does not periodically
reset active jobs.

Authenticated clients can check the stable read-only contract at
`GET /api/v1/provisioning/status`:

```json
{
  "enabled": false,
  "message": "Provisioning is temporarily unavailable for maintenance."
}
```

When provisioning is available, the same endpoint returns:

```json
{
  "enabled": true,
  "message": "Provisioning is available."
}
```

The API synthetic monitor expects this state through
`SYNTHETIC_PROVISIONING_EXPECTED_ENABLED`. While disabled, its normal check set
is read-only; the separately scheduled synthetic janitor may still delete old
synthetic pods because cleanup remains intentionally available.

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

If a workflow genuinely needs more than 60 seconds, **split it** into two
workflows in the playlist. Smaller scripts provide faster student feedback.

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

The **workflow editor catches this at authoring time** (warning **CRU0002**)
when you save a script that calls a command not in the runner image. If you saw
CRU0002 and dismissed it, that is the same root cause surfacing at runtime.

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

## Student content filter policy

Student pods in `10.100.0.0/16` use fwpodv01 for gateway and DNS. The platform
blocks adult/explicit, gambling, drugs, violence, social-media, and streaming
categories; forces SafeSearch; and blocks common external DNS, DoH/DoT/DoQ,
VPN, proxy, and Tor bypass paths. It does **not** intercept TLS.

Global quick logged denies cover TCP/UDP 853, UDP 784 and 8853 (DoQ), UDP 443
(forcing QUIC/HTTP3 fallback), and external GRE, ESP, and AH before broad
per-pod interface passes. Intentional traffic within `10.100.0.0/16` is
preserved for the tunnel protocols. TCP 443 is not blocked globally, so custom
tunnels disguised as ordinary HTTPS remain a residual limitation.

The allowlist is permanent and admin-managed through reviewed deployment
configuration. Instructors and students cannot add temporary exceptions. Send
a false-positive request to a platform admin with the exact domain, course,
reason, and expiry review date; do not tell students to switch DNS or install a
VPN.

Only blocked DNS/firewall events are sent to telemetry. The external log store
retains them for 30 days. Successful browsing and DNS queries are not part of
this policy's telemetry.

### `content_filter_policy` is firing

This synthetic is read-only. It checks the live OPNsense firewall and Unbound
model without creating a pod or changing the firewall. It fails when:

- policy is expected while the current client cannot safely manage and verify
  source-scoped SafeSearch (the built-in switch is global, and management and
  staging must remain unchanged);
- a required global quick rule is missing, duplicated, reordered, unlogged, or
  no longer scoped to `10.100.0.0/16`;
- the source-scoped DNSBL policy, internal category feed, or permanent
  allowlist drifts;
- an uncached controlled query from the student source does not prove the
  effective DNSBL/SafeSearch behavior; or
- policy is expected but the synthetic's dedicated read-only OPNsense
  credentials are absent.

The validated internal feed is
`https://student-filter-feed.lab.jmal.io`. The deployment value
`categoryFeedBaseURL` is this exact base URL, not a list URL; the worker expands
it in order to `/lists/drogue.txt`, `/lists/agressif.txt`,
`/lists/audio-video.txt`, and `/lists/social_networks.txt`. Arbitrary hosts,
ports, paths, queries, fragments, and userinfo are rejected. Its hostname-only LKG inputs are
`drogue` (436), `blacklists/agressif/domains` (266), `audio-video` (3,620),
and `social_networks` (715); IP entries are excluded. OPNsense 26.1 has no
built-in social-media selector. The capacity-tested built-in selection is
exactly `oisd2`, `hgz014`, and `hgz021` (Gambling Mini). Do not substitute the
larger `hgz019` or `hgz020` gambling lists; `hgz022` does not exist. The
supervised 4 GB pilot measured 626,913 final domains and 389 MB Unbound RSS
with this exact selection.

The repository integration is intentionally read-only even if source-scoped
SafeSearch capability becomes available: it validates configuration, inspects
exact firewall/DNSBL state, and verifies effective behavior, but never mutates,
refreshes, or applies the policy. Supervised activation remains a live-only
step until a transactional owner can roll back every firewall, DNSBL, Unbound,
and runtime-verification failure without leaving staged policy behind.
While policy is enabled but inspection is unhealthy, the network reconciler
also suppresses unrelated firewall applies so they cannot activate a partial
staged model.

Do not treat a successful DNSBL API action as proof of runtime enforcement.
Activation is asynchronous, the Python module reloads `dnsbl.json` only on an
uncached query after its 60-second gate, and the action can mask shell failures.

OPNsense does support a source-scoped mechanism outside its built-in switch. A
reversible pilot used an unmanaged
`/usr/local/etc/unbound.opnsense.d/*.conf` fragment with
`access-control-view`, a `view` using `view-first: yes`, and SafeSearch
`local-zone` / `local-data` CNAME rewrites; `configctl unbound check` validated
the result, and removing the fragment restored normal answers. This integration
does not yet transactionally own that fragment, so activation remains blocked.
A follow-up must stage a stable owned fragment, reject conflicts, validate before
reconfigure, roll back and reconfigure on failure, then prove forced answers from
a real student source and unchanged answers from a control source. The synthetic
must repeat the effective uncached student-source check rather than trusting
configuration readback.

The policy must remain disabled until the synthetic can flush/use controlled
uncached fixtures and verify actual answers from `10.100.0.0/16`.

Check `crucible_content_filter_policy` and
`crucible_opnsense_firewall_rules` first. Do not “fix” the alert by disabling
the synthetic or pointing it at the Crucible API; that would be a tautology and
would not inspect the firewall.

### Generated firewall-rule growth

On 2026-08-21, fwpodv01 exposed 3,088 automation rules, 3,078 with blank
descriptions. `/conf/config.xml` had reached 5,272,716 bytes. One semantic rule
(`opt6`, source `10.100.15.0/24`, pass to any) existed 86 times.

The growth had four coupled causes:

1. `CreatePod` added the broad pod pass unconditionally, including on retries.
2. `DestroyPod` removed DHCP/interface/VLAN state but not the pass rule.
3. Reused VLANs and dynamic `optN` assignments therefore accumulated old
   rules.
4. The client sent legacy JSON key `descr`; OPNsense 26.1 expects
   `description`, so generated rules were blank and could not carry ownership.

The repair inventories only `firewall/filter/get` (never `searchRule`), compares
content independent of UUID/description/sequence, and deletes only exact
blank-description legacy or `crucible:pod-pass:v1` shapes. It preserves every
named/manual rule. Malformed, truncated, ambiguous, or over-limit inventories
cause no firewall mutation. Cleanup deletes at most the configured bound and
applies once.

If duplicate/stale counts remain positive, leave the worker controlled and let
subsequent supervised passes converge. Never bulk-delete all blank rules by
description alone.

### Alert rules owned by the observability deployment

This repository emits the metric/check contract but does not provision live
Grafana resources. The observability owner should install:

```promql
1 - crucible_synthetic_check_success{check="content_filter_policy"} > 0
```

```promql
crucible_opnsense_firewall_rules{kind=~"duplicate|stale"} > 0
or
crucible_opnsense_firewall_cleanup_limited == 1
```

```promql
delta(crucible_opnsense_firewall_rules{kind="total"}[15m]) > 25
```

```promql
(crucible_content_filter_policy{kind="expected"} == 1)
and on(job, component, layer)
(crucible_content_filter_policy{kind="healthy"} == 0)
```

```promql
absent(crucible_content_filter_last_success_timestamp_seconds)
or
(time() - crucible_content_filter_last_success_timestamp_seconds > 15 * 60)
or
(time() - crucible_network_reconcile_run_timestamp_seconds > 15 * 60)
```

Use this section as the runbook URL. Do not enable the policy or alerts until
the internal validated category feed, read-only synthetic identity, external
30-day blocked-only retention, and supervised rollout are ready.

---

If you've checked the above and the workflow still misbehaves, capture:

1. The workflow JSON (`GET /api/v1/admin/workflows/{slug}`).
2. The failing run's full instructor output.
3. The output of `env | grep CRUCIBLE_` from a successful test run.
4. Whether it ever worked, and what changed.

Then escalate to the platform admin. With those four things, root
cause usually falls out in minutes.

---

## "Provision is disabled — the wizard says preflight checks failed"

Before a clone job is enqueued, the wizard runs a set of **preflight checks**
that verify the environment is ready. If any *blocking* check fails, the
Provision button is disabled and each failing row shows a **Fix** hint.

Common causes:

**Source VM moref does not resolve (PF-01)**
: The source VM was deleted or moved in vCenter after the draft was created.
  Re-run the draft step and pick a valid source.

**Target datastore not mounted on all cluster hosts (PF-03)**
: The configured datastore isn't accessible from the full cluster. Either
  mount it on the missing hosts, or contact the platform admin to update
  the target datastore in the Crucible config.

**Not enough datastore free space (PF-04)**
: Free space < source provisioned size × 1.2. Delete stale staging VMs or
  snapshots to recover space, then re-run the checks.

**In-flight tasks on source VM (PF-06)**
: Another clone or consolidation is already running against the same source.
  Wait for it to finish (check vCenter Tasks), then try again. This check
  exists because concurrent operations against the same source VM are the
  leading theory for certain intermittent clone failures.

**VMware Tools not running (PF-07)**
: The source VM must be powered on with Tools running before a
  clone-with-customize succeeds. Power it on, wait for Tools to start, then
  re-run the checks.

**Target VM name already taken (PF-09)**
: A VM with the generated name already exists in the Templates folder — likely
  a leftover from a previous failed provision. Delete the stale VM in vCenter,
  then re-run.

**Warning rows (amber ⚠)** do not block the Provision button; they flag
potential issues (missing guest credentials, staging port group not found on
standard vSwitch) that you may want to investigate but are not fatal.

If all checks are green but provisioning still fails immediately, an
*intermittent* vCenter fault may be in play. Static checks cannot detect those
faults; the platform automatically retries them with backoff.

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

**VMware Tools showing "running" during an ISO install does not signal
completion.** The Ubuntu Server installer runs Tools inside its own live
environment about 40 seconds after power-on, with a completely empty disk.
Crucible deliberately waits for the VM to **power itself off**, which the
generated config does when the install genuinely completes. Do not use Tools
status to judge installation progress.

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

**On Linux the cleanup script does not power the guest off.** That is
deliberate, and it is what makes completion provable:

1. A guest that powers itself off kills the VMware Tools agent *while the
   platform is still talking to it*, so the guest-ops call returns an error
   even on a perfect run. The message vCenter returns is:

   ```
   ServerFaultCode: The guest operations agent could not be contacted.
   ```

   That exact message is also what you get when VMware Tools **never started
   at all**, so it cannot be used to tell success from failure.

2. `guestinfo` variables written by the guest live only in the **running** VM's
   configuration — **vCenter clears them when the VM powers off.** So a marker
   stamped by the script and then followed by a shutdown is erased by the very
   shutdown it was meant to survive.

Because the script simply exits, Crucible gets a real exit code back. It then
corroborates that with a marker the script stamps as its final line, read
**while the guest is still powered on**:

```bash
vmware-rpctool "info-set guestinfo.crucible.generalize.job <job-id>"
```

Only after both signals agree does the platform issue the shutdown itself.
Because the value is the **job ID** and not a fixed word, a marker left over
from an earlier generalize attempt on the same VM cannot be mistaken for this
one.

What that means for you:

| Template state after Generalize | What it tells you |
|---|---|
| `ready` | The cleanup script ran to its final line and the marker was confirmed. Trustworthy. |
| `error`, *"generalize script failed"* | The script returned a non-zero exit code — usually the `sudo` problem above. The message carries the guest's own error. Fix it and re-run Generalize; do not publish. |
| `error`, *"never stamped the completion sentinel"* | The script reported success but left no marker, so the cleanup cannot be shown to have run. Re-run Generalize; do not publish. |

Windows is exempt from the marker. Sysprep is launched fire-and-forget and
powers the machine off on its own schedule, so there is no opportunity to
stamp or read a marker. Windows generalize still relies on the shutdown signal.

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
caution in [Templates](templates.md). A `ConditionPathExists` guard on
the offending unit does not help: systemd resolves ordering cycles *before*
it evaluates conditions, so a unit that gets skipped anyway can still take
SSH down.

Report it if a freshly-provisioned template shows this — the generator writes
that unit, so it is a platform bug rather than something you did.

---

## "Template provisioning failed, but then it succeeded / failed again after a delay"

Template creation jobs (clone, generalise, etc.) are automatically retried up
to **three times** when vSphere returns a transient error — for example, a
storage-inventory rejection that resolves itself in under a minute. While
retries are in flight the template shows `pending`, and each attempt is
recorded in the platform job log.

What this looks like in practice:

- You request a template at 20:36 — vSphere rejects the clone in under a
  second with a storage/inventory error.
- The platform schedules a retry with exponential backoff (roughly 30 s, then
  2 min, then 8 min).
- By 20:37 the second attempt succeeds and the template continues to
  `configuring`.

If all three retries exhaust, the job moves to `error` and the status page
shows a message like *"vSphere rejected the clone (transient storage/inventory
error). Retried 3 times. If this persists, check datastore health."* The raw
vCenter fault string is included for platform admins in the job detail view.

**Errors that are never retried** (they indicate a configuration mistake, not
a transient fault): invalid source ISO path, missing folder, ambiguous
resource pool, guest-auth failure. These fail immediately so you see the real
cause without waiting through three backoff cycles.

If a template stays in `pending` longer than expected after an error, it is
likely in a retry backoff window. Check the Jobs page for the next-attempt time.
Only intervene (reset to `error`) for a deterministic configuration error, not
a transient one.

---

## See also

- [Building Workflows](workflows.md) — the basics
- [Runner Environment](runner-environment.md) — what's available at runtime
- [Glossary](glossary.md) — quick term lookup
