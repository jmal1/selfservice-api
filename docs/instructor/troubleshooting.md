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

### Rollback-safe immutable baseline before a phase-1 upgrade

A live Deployment override is not a rollback control. Helm rollback restores
the previous revision's rendered environment, so a prior revision with
`WORKER_PROVISIONING_CLAIMS_ENABLED=true` can briefly claim queued work even
when the live pod was patched to `false`. Likewise, a stored Helm manifest is
not proof that applying it will avoid restarts: live
`kubectl rollout restart` annotations may be absent from Helm, and a restart of
a workload whose stored image is `:latest` can pull different code.

Before pulling upgrade code, building or pushing images, or running a migration:

1. Preserve a clean checkout of the chart revision currently running safely in
   production.
2. From the hotfix checkout, create a Helm baseline revision using that safe
   chart. The deploy host must have `jq`, `kubectl` configured for production,
   and Helm 3.14+:

   ```bash
   ./deploy/scripts/deploy.sh \
     --prepare-claims-baseline \
     --baseline-chart-dir /path/to/safe-checkout/deploy/helm/selfservice
   ```

   The command uses the release's existing values and changes
   `provisioning.workerClaimsEnabled=false`. It inventories every image-bearing
   Deployment, DaemonSet, StatefulSet, CronJob, and Job rendered by those
   production values. This includes the API, worker, engine, UI, synthetic
   monitor, runner warmer, subchart workloads, and any future rendered
   workload. Every container and init-container is pinned to the single
   immutable sha256 ImageID reported by its healthy live pod. The engine's
   `RUNNER_IMAGE` is pinned to the same digest as the runner warmer. Missing
   pods or retained CronJob evidence, mutable/non-sha256 ImageIDs, mixed
   digests, container inventory changes, or repository mismatches stop the
   command before Helm mutation.

   Before applying, the command compares each rendered workload directly with
   the live resource; it does not compare only stored Helm manifests and does
   not use three-way `kubectl diff`. The candidate is submitted under a
   temporary name with server-side dry-run defaulting, then its canonical spec
   is compared directly with the canonical live spec. This ensures live-only
   command, environment, volume, or security settings cannot be silently
   preserved by apply merge semantics. Image references are normalized back to
   the proven live declarations for this drift check. The only benign live
   difference is
   `kubectl.kubernetes.io/restartedAt`. Removing that annotation may restart a
   pod, but is allowed only after every candidate pin is proven equivalent to
   the effective live digest. Any command, environment, volume,
   security-context, replica, service-account, or other workload drift fails
   closed.

   The baseline upgrade does not use `--atomic`: a failed baseline must never
   roll back to the claims-enabled revision. Its failure path reasserts the
   live claims override and stops. On success, it verifies the newest Helm
   revision is deployed, persisted and live claims remain false, the live
   worker count is exactly one, every declared image and effective ImageID
   equals the persisted pin, all workload rollouts and retained jobs are
   healthy, the synthetic monitor remains healthy, and the database migration
   is unchanged. Fix any failure and rerun the baseline before proceeding.
3. Re-run the read-only proof:

   ```bash
   ./deploy/scripts/deploy.sh --verify-rollback-containment
   ```

PostgreSQL must report migration **35 clean**. The script checks that against
the latest migration in the hotfix checkout and proves the version and dirty
flag are unchanged across baseline preparation. Stop for incident recovery if
it reports another state.

Only after both commands succeed may the operator update the checkout/images
and run the normal phase-1 deployment. The normal deploy path repeats the full
Helm/live proof before `git pull`, records the exact baseline revision and
migration state, then repeats the proof after pull and dependency resolution.
An intervening Helm revision is rejected, so the immutable all-workload
baseline remains the immediately previous successful revision used by
`helm upgrade --atomic`. If any part fails, the script exits before the
application upgrade.

Both baseline creation and the final application-upgrade check hold the
cluster-visible ConfigMap lock
`selfservice/selfservice-phase1-deploy-lock`. Every release mutation during
this procedure must go through `deploy.sh`; do not run an independent
`helm upgrade` that ignores the lock. If a terminated deploy leaves the lock
behind, inspect its `crucible.jmal.io/holder` annotation and prove that holder
is no longer active before deleting it. Do not use `kubectl set env` as a
substitute and do not manually select an older or mutable revision for
rollback.

Operators can distinguish ordinary queued work from recovery work in the job
payload. Pod/VM creation, template staging/generalization/verification/health,
and image-import jobs are withheld while provisioning claims are disabled
unless they carry `cleanup_only: true`. Before submitting a clone, the worker
persists a per-attempt operation UUID, the selected host and pool identities,
and embeds those identities with the source template and pod VM identity (or
job/template scope for a smoke clone) in vCenter `extraConfig`. The returned
task MoRef is persisted before waiting. A
replacement worker resumes that exact task, or reconciles a lost SOAP response
by the complete marker and target name; it never submits a second clone or
adopts a same-name VM without matching ownership proof. The exact resulting VM
MoRef is then staged before any reconfiguration. Cleanup never resolves a VM by
display name and never resumes forward configuration, power-on, or snapshots.

Clone task waits have a 15-minute operational deadline. A transient persisted
task wait before clone acceptance resumes that same task within the job's
forward retry budget; it never submits another clone. Once the exact VM is
accepted and staged, placement validation or configuration failure switches
immediately to cleanup-only compensation. Recovery destroys the exact marked VM
and cannot replay VLAN, interface, DHCP, firewall, portgroup, or clone setup. An armed submission
with no task reference and no discoverable marked VM remains cleanup-only for
30 minutes, then surfaces `manual_cleanup_required` for operator resolution
rather than retrying or cloning indefinitely. Pod creation keeps exact clone
cleanup intent armed until its rollback record is durable and the same claim
finishes adoption.
Failed cleanup remains pending with capped backoff independently of the
original provisioning retry limit. Successful compensation records the parent
job as failed with `compensated: true`; ambiguous ownership records
`manual_cleanup_required: true` and requires operator resolution rather than
deleting or adopting an uncertain VM by name.

A transient database failure while rescheduling compensation does not strand
the job on a live worker. The worker retries the durable pending-state write
with bounded backoff until it succeeds. Each worker process owns jobs with a
unique process identity plus a per-claim fencing token and refreshes a heartbeat
lease. Startup and periodic recovery reset only expired claims, never fresh
work owned by another replica; ownership loss cancels execution and blocks
stale finalization. On shutdown, unfinished claims become recoverable only
after their conservative lease expires. If ownership changes while a completed
infrastructure step is being recorded, the stale worker does not undo it behind
the successor. Any ambiguous persistence failure uses the same handoff path.
Before every destructive rollback step, the worker re-persists the receipt under
its active claim and refreshes the lease; failure to prove ownership stops the
rollback. The handoff appends the exact receipt, fences the current generation
into cleanup-only work, and leaves destructive compensation to the next owner.
Worker shutdown, including scheduler and database-pool closure, is bounded at
two minutes, with 150 seconds of pod termination grace for that handoff.

### "No eligible allowlisted vCenter placement"

This message is a safety block, not a transient DRS hint. `VCENTER_HOSTS` is the
only host allowlist for both VM placement and standard-vSwitch portgroup
changes. A candidate must also be connected, outside maintenance mode, in the
configured/source-compatible resource pool, able to access the datastore, have
the required standard portgroup, and satisfy the VM's capacity requirements.
For templates with source replicas, the source VM, resource pool, and target
host must have the same immutable compute-resource identity. Once a template has
ever had a replica registered, an unavailable, unhealthy, or deleted replica
never falls back to the legacy source. Check the `replica_mode` field as well as
the `replicas` array; `replica_mode: true` with no `ready` entries intentionally
blocks provisioning until a replacement source is registered.

An in-flight legacy job that already adopted a clone before the placement
migration reconstructs the VM's exact live host, configured resource pool, and
compute identity, then installs the per-VM DRS override before continuing. This
does not guess a new destination or charge the already-resident VM against
headroom again. If source-replica mode was enabled before that reconstruction,
the job fails closed because the original replica identity cannot be proven.

PF-03 and PF-11 use this same resolver, so a failed preflight cannot be
overridden by cluster-wide availability elsewhere.

Do not add another host or broaden `VCENTER_HOSTS` merely to clear the error.
Check the named host's connection and maintenance state, resource-pool
membership, datastore mount, standard portgroup, and available capacity. Check
`GET /api/v1/admin/templates/{templateID}/source-replicas` for a `ready` source
in the intended compute resource. A diagnostic mentioning reserved headroom
means the host would fall below
`VCENTER_PLACEMENT_RESERVED_MEMORY_MB` after the new VM and the other VMs in the
same pod plan. Durable reservations from other unreleased placement plans are
included under a per-host PostgreSQL admission lock, so concurrent workers
cannot overbook the reserve. Reservations are released explicitly only after a
VM is running or exact compensation proves no VM remains. A terminal or
`manual_cleanup_required` job can therefore continue reserving capacity by
design; resolve its exact cleanup state rather than editing the job status. Do
not reduce the reserve without an explicit capacity review.
During an isolated-host canary, keep API admission and worker provisioning
claims off, keep worker replicas at zero until the approved step, and keep
lifecycle, runner, template-health, L1, and other destructive
synthetics/schedulers off.

Crucible installs a per-VM DRS-disabled override and checks the persisted host,
compute resource, and override before later forward operations. A missing
override, enabled override, or moved VM is placement drift and the operation
fails closed. Inspect `crucible_vm_placement_drift_total{kind=...}` and the
worker error; restore the exact placement/control only after confirming the VM
identity. Before networking or cloning, the worker checks
`Host.Inventory.EditCluster` on every selected `ClusterComputeResource`.
Grant this privilege to the Crucible service account on the selected cluster
with the narrowest practical scope; do not remove the DRS override to bypass a
permission failure. A timeout, connection error, or failed vCenter property read is not
proof of drift and remains eligible for normal job retry instead of being
terminalized as manual cleanup. Destructive clone cleanup also compares the
live resource pool and durable source, replica, template, pod-VM, compute, pool,
and host markers. A pre-configuration clone may lack its DRS override, so exact
cleanup installs and verifies the missing control before deletion; an enabled
override remains confirmed drift. VM rows remain non-terminal until exact
cleanup succeeds, preserving the identity needed for safe retries. Manual vMotion and the
clone-to-override interval still require an
external VM-host affinity/must-run rule or equivalent host exclusion during a
containment canary. For the ESXi2 canary, `VCENTER_HOSTS` must contain ESXi1
only, the resource pool must be the compatible AMD pool, and the external rule
must keep Crucible and temporary VMs off ESXi2. Leave the pending production
pod/job untouched until a human approves the canary.

Placement diagnostics are exported as:

| Metric | Meaning |
|--------|---------|
| `crucible_vm_placement_total{host,compute,source}` | Durable placement decisions by exact target and source replica (`source="legacy"` for zero-row compatibility). |
| `crucible_vm_placement_headroom_megabytes{host}` | Free memory remaining after the pod plan and configured reserve. |
| `crucible_vm_placement_rejections_total{reason}` | Failed placement decisions, including `reserved_headroom`. |
| `crucible_vm_placement_drift_total{kind}` | Refused operations after `host`, `compute`, or `drs` drift. |

### "Template replica build is stuck or requires cleanup"

Read the durable operation first:

```http
GET /api/v1/admin/templates/{templateID}/source-replica-builds/{buildID}
```

Correlate `phase`, task MoRefs, exact VM MoRefs, `last_error_code`, and
`last_error` with vCenter task history and the current worker claim. A timeout
after a `*_submitting` phase is not proof of failure: the worker searches for
the exact build/operation/kind marker before deciding whether a task was
accepted. Do not enqueue a new idempotency key or clone by hand while that
lineage is unresolved.

Use `/retry` only for `status=failed`; it resumes the stored forward-only
`resume_phase`. Cleanup failures do not replace that checkpoint. Use `/cleanup`
for failed or `cleanup_required` operations only after confirming the stored
ownership. The same endpoint retires a `ready` build: it rejects placement
references and non-retired downstream builds that use the result as an anchor,
requires the distinct source anchor to remain ready, disables the result replica
for new placement, and destroys only the marker-owned result VM. The source
anchor is never destroyed. A duplicate recovery request returns `409` while the
linked job remains active. Template deletion also returns `409` if a build is
admitted or restarted before the deletion lock is acquired; neither operation
can race the subsequent vCenter destroy. Fully cleaned failed downstream builds
release historical anchor references automatically; an uncleaned build remains
a hard conflict. Forward retry returns `409` if its exact source anchor is no
longer ready.

Cleanup verifies the exact MoRef and all operation markers before deletion. If a
destroy response is lost, a successor revalidates that exact identity and may
safely resubmit only the destroy; clone, snapshot, and linked-clone creation
remain never-resubmit operations. A same-name VM, missing marker, duplicate
marker, or property read failure must be escalated rather than
deleted. `ready` is impossible until the linked-clone canary cleanup timestamp is
persisted and no marked canary residue remains.

After exact retained cleanup, the build reloads with `phase=residue_cleaned`,
`residue_cleaned_at` set, and no result-replica reservation. Start a new build
with a new idempotency key to retry that compute. Do not edit replica rows
manually.

See [Durable retained replica builds](templates.md#durable-retained-replica-builds)
for API payloads, privilege requirements, metrics, and alert rules.

### "Durable port group ownership cannot be proven"

Destroy never guesses which host portgroups a pod owns. The worker persists an
independent per-pod ledger before switch mutation and captures each exact
`HostPortGroup.Key` before the receipt becomes active. Monotonic `planned`,
`applying`, and `active` states prove whether mutation was authorized; a
keyless `applying` receipt with no matching group is intentionally ambiguous.
Pre-ledger rollback receipts are backfilled in a fail-closed `legacy` state and
cannot adopt a current same-name portgroup as ownership proof. Successful
rollback records a durable `removed` tombstone that survives rollback-step
checkpointing and makes a later destroy idempotent. A changed key, name, VLAN,
`vSwitch0`, or inherited security
policy; a missing or malformed receipt; a legacy receipt without immutable host
identities; or a receipt naming a host outside the current `VCENTER_HOSTS`
allowlist makes the destroy job report
`manual_cleanup_required`. The pod remains `destroy_failed`, and its
VLAN/interface allocation remains reserved so the VLAN cannot be reused while a
portgroup may still exist.

Do not widen `VCENTER_HOSTS` or infer ownership from OPNsense VLAN state to clear
this condition. An operator must inspect the historical job and vCenter state,
then either backfill an exact per-host receipt with reliable ownership evidence
or complete the cleanup manually. A transient `RemovePortGroup` failure follows
the same resource-retention rule but remains retryable.

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
`/lists/dangerous_material.txt`, `/lists/audio-video.txt`,
`/lists/social_networks.txt`, and `/lists/weapons.txt`. Arbitrary hosts, ports,
paths, queries, fragments, and userinfo are rejected. These are the live
schema-2 custom list categories; IP entries are excluded. OPNsense 26.1 has no
built-in social-media selector. The capacity-tested built-in selection remains
exactly `oisd2`, `hgz014`, and `hgz021` (Gambling Mini). Do not substitute the
larger `hgz019` or `hgz020` gambling lists; `hgz022` does not exist.

The repository integration remains intentionally read-only: it validates
configuration and inspects exact firewall/DNSBL state, but never mutates,
refreshes, or applies the policy. The transactional source-scoped SafeSearch
fragment owner is a foundation API only and is not called by
`reconcileContentFilter`. Supervised activation remains blocked until one
transaction can roll back every firewall, DNSBL, Unbound, and
runtime-verification failure without leaving staged policy behind.
While policy is enabled but inspection is unhealthy, the network reconciler
also suppresses unrelated firewall applies so they cannot activate a partial
staged model.

Do not treat a successful DNSBL API action as proof of runtime enforcement.
Activation is asynchronous, the Python module reloads `dnsbl.json` only on an
uncached query after its 60-second gate, and the action can mask shell failures.

OPNsense does support a source-scoped mechanism outside its built-in switch. A
reversible pilot used a dedicated
`/usr/local/etc/unbound.opnsense.d/*.conf` fragment with
`access-control-view`, a `view` using `view-first: yes`, and SafeSearch
`local-zone` / `local-data` CNAME rewrites; `configctl unbound check` validated
the result, and removing the fragment restored normal answers.

Crucible now owns only
`/usr/local/etc/unbound.opnsense.d/crucible-student-safesearch.conf`; it never
edits generated global `safesearch.conf` or another manual fragment. The worker
requires `OPNSENSE_SSH_HOST_KEY` as one OpenSSH public-key line
(`ssh-ed25519 AAAA...`, optionally followed by a comment). The Helm chart takes
it from `opnsense.sshHostKey` when explicitly set, otherwise from key
`opnsense-ssh-host-key` in `secrets.opnsense`. Obtain and compare it through a
trusted OPNsense console or previously authenticated channel; do not trust a
first-use network scan.

The owner backs up both owned copies, rejects unsafe CIDRs and conflicting
manual views, writes atomically, stages the chroot copy at
`/var/unbound/etc/crucible-student-safesearch.conf`, validates with
`configctl unbound check` plus direct `unbound-checkconf`, and activates through
the supported `configctl unbound restart` action followed by a running-service
check. This extra parsing is required because `configctl` can mask an underlying
script failure behind exit status 0. Every post-write failure restores or
removes both copies, validates, and restarts again. Missing, malformed, or
mismatched host-key pins fail closed. This does **not** enable content
filtering: the reconciler remains read-only, and the synthetic must still prove
effective uncached answers from a real student source plus unchanged answers
from a control source rather than trusting configuration readback.

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
