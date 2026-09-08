# Building Workflows

A **workflow** is one bash script that grades one thing. It's the unit
of work students see in their assessment results.

Read the [Overview](overview.md) first if you have not already; it explains how
workflows fit alongside actions, playlists, and pods.

---

## The shape of a workflow

Every workflow is a single bash script that runs to completion in **one
process**. The engine writes your `script` field to disk on the runner
and runs `bash` against it.

A minimal workflow looks like this:

```bash
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "telnet-blocked" bash -c '
    if nc -zw3 "$CRUCIBLE_TARGET_IP" 23; then
        echo "STUDENT_MSG: Telnet is still reachable on port 23."
        echo "STUDENT_MSG: Block it with: sudo ufw deny 23/tcp"
        exit 1
    fi
'
```

Three things to notice:

1. **`source /opt/crucible/lib/actions.sh`** — pulls in the `run_action`
   helper. Always at the top.
2. **`set -uo pipefail`** — fail loud on unset variables and broken pipes.
   Saves you from "passed because grep printed nothing".

   Note this is **not** `set -euo pipefail`. `run_action` returns the check's
   exit code, so under `set -e` the first failing check ends the whole
   workflow: every later check never runs and never reports, and the student
   sees one red result followed by silence instead of a full list. Nothing in
   the output looks like an abort — the run simply has fewer results than the
   workflow has checks.
3. **`run_action "<display label>" <command-or-function> [args...]`** — every grading step is wrapped in
   `run_action` so the engine can capture per-step pass/fail, output,
   and duration. The label and executable are separate required arguments.

A workflow with no `run_action` calls is treated as a single anonymous step.
Use explicit `run_action` blocks to give students named, individual line items
in the result UI.

---

## The full JSON payload

Workflows are created via `POST /api/v1/admin/workflows`. The admin UI
is a thin wrapper around this endpoint.

```json
{
  "name": "Telnet Blocked",
  "slug": "telnet-blocked",
  "description": "Asserts that TCP/23 on the target VM is closed.",
  "category": "hardening",
  "execution_mode": "kali_runner",
  "creation_mode": "script",
  "timeout_seconds": 60,
  "target_os": "linux",
  "script": "source /opt/crucible/lib/actions.sh\nset -uo pipefail\nrun_action \"port-23-closed\" bash -c '...'",
  "visible_to_students": true,
  "status": "draft"
}
```

### Field reference

| Field | Required | Notes |
|---|---|---|
| `name` | yes | Human-readable. Shown to students if `visible_to_students` is true. |
| `slug` | yes | `[a-z0-9-]+`, globally unique. Used in URLs and runner config. |
| `description` | no | Markdown. Visible in the admin UI. |
| `category` | no | Freeform tag; common ones: `hardening`, `networking`, `forensics`, `recon`. |
| `execution_mode` | no | `kali_runner` (default) or `vmware_tools`. See [Overview](overview.md). |
| `creation_mode` | no | `visual` (built via UI) or `script` (raw bash). |
| `script` | yes for `script` mode | The bash body. Start with `source /opt/crucible/lib/actions.sh`. |
| `setup_script` | no | Runs once before `script`. Hard 60s timeout. |
| `timeout_seconds` | no | Hard ceiling for the whole script. Default 300. |
| `target_os` | no | Hint only (`linux`, `windows`). |
| `target_vm` | no | Slot name within the pod if multiple targets. |
| `guest_interpreter` | no | Only for `vmware_tools`: `/bin/bash`, `cmd.exe`, `powershell.exe`. |
| `required_services` | no | Engine preflight, e.g. `["vcenter", "nats"]`. |
| `visible_to_students` | no | If false, name + output stay hidden from students. |
| `status` | no | Starts as `draft`. See lifecycle below. |

The full schema lives in
[`internal/models/workflow_models.go`](../../internal/models/workflow_models.go)
and is enforced by Postgres CHECK constraints in
[`internal/database/migrations/000012_assessment_engine.up.sql`](../../internal/database/migrations/000012_assessment_engine.up.sql).

---

## STUDENT_MSG and instructor output

By default everything your script prints is **instructor-only** — useful
for debugging, never shown to the student. The single exception: lines
prefixed with `STUDENT_MSG:` are surfaced in the student's result panel.

```bash
echo "STUDENT_MSG: Make sure /etc/ssh/sshd_config has PasswordAuthentication no"
echo "DEBUG: sshd_config dump:"  # instructor-only
cat /etc/ssh/sshd_config        # instructor-only
```

> [!tip]
> Treat STUDENT_MSG as a learning opportunity. Don't just say "failed" —
> tell the student *what* they're missing and *how* to fix it. Good
> messages turn an assessment into a tutor.

---

## Patterns: composing actions

If you're using the library, call the generated shell function instead of
inlining bash. Library action slugs are kebab-case in the database, while runner
callables replace each hyphen with an underscore: `service-running` becomes
`service_running`.

```bash
source /opt/crucible/lib/actions.sh
set -uo pipefail

# Library action: checks systemd unit state via SSH to the target
run_action "SSH service is active" service_running --name ssh
run_action "UFW service is active" service_running --name ufw

# Inline check that doesn't deserve a library entry
run_action "audit-log-exists" bash -c '
    ssh "$CRUCIBLE_TARGET_USERNAME@$CRUCIBLE_TARGET_IP" \
        "test -f /var/log/auth.log" || {
            echo "STUDENT_MSG: /var/log/auth.log is missing — is auditd running?"
            exit 1
        }
'
```

The first argument is the result label shown to instructors and students. The
second is what the runner executes. These are invalid:

```bash
# Missing the command/function argument
run_action "demo-http-service-reachable"

# Uses the database slug where the generated function is required
run_action "DEMO - HTTP Service Reachable" demo-http-service-reachable
```

The valid library call is:

```bash
run_action "DEMO - HTTP Service Reachable" demo_http_service_reachable
```

When mixing library actions and inline checks, the **ordering still matters**:
failures abort the workflow because of `set -e`. Put setup steps first, then
your most important assertion, then nice-to-have follow-ups.

---

## Lifecycle: draft → pending → approved → active

A new workflow starts as `draft`. It can't be added to a playlist run
until it reaches `active`.

```
draft  →  pending_review  →  approved  →  active
```

Transition via the admin UI Workflows page, or:

- `PATCH /api/v1/admin/workflows/{id}/submit`  (draft → pending_review)
- `PATCH /api/v1/admin/workflows/{id}/approve` (pending_review → approved, **admin only**)
- `PATCH /api/v1/admin/workflows/{id}/activate` (approved → active)

Activation re-reads the persisted script and current database action library.
It returns `422 Unprocessable Entity` if a `run_action` omits its second
command/function argument or uses a known kebab-case library slug instead of
the generated snake-case callable. The error identifies the line and expected
callable, so imported and hand-written scripts cannot bypass the guard.

Editing a `pending_review`, `approved`, or `active` workflow returns that
workflow to `draft` and clears its prior approval. It is excluded from new
playlist runs until it passes review and activation again. A run that already
started keeps its immutable launch snapshot, so the edit cannot change work
already in flight.

---

## Testing your workflow

You have three options, in order of fidelity:

1. **Local bash sanity check** — paste the script into a shell and
   substitute the env vars. Fast feedback for syntax/logic.
2. **Dry run on a known-good pod** — deploy your own pod from the same
   blueprint students will use, then `Run` your workflow against it.
   This catches missing dependencies on the runner.
3. **Run against a known-bad pod** — confirm it actually fails when the
   student hasn't done the work. Easy to forget; this is how you avoid
   the "always passes" bug.

> [!caution]
> Always do step 3. A workflow that never fails is worse than no
> assessment — students think they've succeeded when they haven't.

---

## Common pitfalls

| Symptom | Likely cause |
|---|---|
| Always passes, even when student hasn't done the work | Missing `set -e`; or test command silently succeeds (e.g. `grep || true`) |
| Times out at 300s for no apparent reason | SSH hang to target — add `-o ConnectTimeout=5` |
| "command not found: run_action" | Forgot the `source /opt/crucible/lib/actions.sh` line |
| Activation rejects `demo-http-service-reachable` and suggests `demo_http_service_reachable` | The library slug was passed as the command; keep the label first and use the generated snake-case function second |
| Student sees no message on failure | No `STUDENT_MSG:` lines; only debug output |
| Workflow disappeared from new playlist runs after an edit | Reviewed content returns to `draft`; submit, approve, and activate the edited workflow again |

More in [Troubleshooting](troubleshooting.md).

---

## See also

- [Building Actions](actions.md) — when to extract into the library
- [Runner Environment](runner-environment.md) — what env vars and tools you have
- [Playlists](playlists.md) — how to bundle workflows into a graded lab
- [AGENTS.md](../../AGENTS.md) — full schema + AI-ready authoring reference
