# Building Library Actions

An **action** is a reusable grading step. If you find yourself copying the
same `run_action "..." bash -c '...'` block into three workflows, it's time to
promote it to the library.

Read [Building Workflows](workflows.md) first if you have not already. Actions
make sense once you have felt the cost of repeating yourself.

> [!important]
> This page documents what the runner **actually does**. Some fields the API
> accepts are stored and never read — they are listed under
> [Accepted but inert](#accepted-but-inert). Do not author against them.

---

## When to write an action (vs. inline bash)

| Situation | Use |
|---|---|
| One-off check, specific to a single assessment | **Inline** `run_action "<name>" bash -c '...'` |
| Same logic in 2+ workflows | **Library action** taking `--flag` arguments |
| You want students to see consistent error messages across labs | **Library action** setting `LAST_STUDENT_MSG` |

Wait for the third copy-paste before extracting. Premature library growth becomes
the next person's discovery problem.

---

## The shape of an action

Actions are created via `POST /api/v1/admin/actions`. Like workflows,
the admin UI is a thin wrapper.

```json
{
  "name": "Systemd Service Running",
  "slug": "service-running",
  "description": "Asserts the named systemd unit is active.",
  "action_type": "service_check",
  "action_category": "service",
  "supported_platforms": ["linux:ubuntu", "linux:debian"],
  "input_context": [
    {"key": "name", "type": "string", "description": "systemd service name"}
  ],
  "output_context": [
    {"key": "service_NAME_status", "type": "string", "description": "Service status (active)"}
  ],
  "script": "local name=\"\"\nwhile [[ $# -gt 0 ]]; do\n    case \"$1\" in\n        --name) name=\"$2\"; shift 2;;\n        *) shift;;\n    esac\ndone\nif systemctl is-active --quiet \"$name\" 2>/dev/null; then\n    ctx_set \"service_${name}_status\" \"active\"\n    return 0\nelse\n    LAST_ERROR=\"Service $name is not running\"\n    LAST_STUDENT_MSG=\"Service $name is not running. Start it with: sudo systemctl start $name\"\n    return 1\nfi"
}
```

That is the real body of the shipped `service-running` action, and it shows the
three conventions every library action follows: a `--flag value` argument loop,
`ctx_set` for data other steps may want, and `LAST_ERROR` /
`LAST_STUDENT_MSG` plus a non-zero `return` for failure.

### Field reference

| Field | Required | Notes |
|---|---|---|
| `name` | yes | Human-readable; shown in the action picker. |
| `slug` | yes | `[a-z0-9-]+`, globally unique. The runner exports it as a shell function with `-` replaced by `_`. |
| `description` | yes | One sentence. Shown in the picker. |
| `action_type` | no | Free text, defaults to `command`. **The runner ignores it** — dispatch is purely "is there a shell function by this name". It is a UI label only. In-use values: `command`, `dns`, `file_check`, `http`, `port_check`, `service_check`, `ssh`. |
| `action_category` | no | Free text, defaults to `general`. Groups the picker. In-use values: `general`, `network`, `file`, `service`, `firewall`, `ssh`. |
| `supported_platforms` | no | Array of platform tags, defaults to `["any"]`. Only the `windows` tag changes behaviour; see [Platform tags](#platform-tags--why-they-matter). |
| `input_context` | no | Array of `{key, type, description}` objects declaring what the body reads. Used by the script validator, not injected. See below. |
| `output_context` | no | Array of `{key, type, description}` objects declaring what the body writes with `ctx_set`. Documentation + lint. |
| `script` | yes | The bash body. Runs as a shell function, so top-level `local` is allowed. |

There is **no database constraint** on `action_type` or `action_category`; a
typo will be accepted silently and only show up as a stray group in the picker.
Copy an existing value.

Full schema: [AGENTS.md](../../AGENTS.md) §3.2.

---

## Arguments: how they're passed in

Arguments are ordinary shell arguments. The workflow passes `--flag value`
pairs after the callable, and the body parses them with a `while` loop:

```bash
# In the workflow:
run_action "SSH service is active" service_running --name ssh
```

```bash
# In the action body — parse every flag you accept, and ignore the rest:
local name="" timeout=5
while [[ $# -gt 0 ]]; do
    case "$1" in
        --name) name="$2"; shift 2;;
        --timeout) timeout="$2"; shift 2;;
        *) shift;;
    esac
done
```

Declare each flag in `input_context` so the editor knows about it and other
instructors can discover it without reading the body. The declaration does not
create the variable — your parse loop does.

`run_action` always takes a **display label first** and a **callable second**.
The action's database slug is `service-running`, but its generated shell
function is `service_running`. The label can contain spaces and is what appears
in results.

```bash
# Correct
run_action "DEMO - HTTP Service Reachable" demo_http_service_reachable

# Invalid: no callable after the label
run_action "demo-http-service-reachable"

# Invalid: the database slug is not the generated function name
run_action "DEMO - HTTP Service Reachable" demo-http-service-reachable
```

Activation checks the persisted workflow against the current action library and
reports the expected snake-case callable before the workflow can become active.

> [!warning]
> Argument values are passed through the shell unchanged. **Never `eval`**
> one. Quote every expansion: `systemctl is-active --quiet "$name"`.

---

## Reporting failure

A library action reports failure by assigning two variables and returning
non-zero:

```bash
LAST_ERROR="Port $port is not open on $host"           # instructor-only
LAST_STUDENT_MSG="Port $port is not responding on $host. Is the service running?"
return 1
```

`run_action` re-emits them as `STUDENT_MSG:` and `ERROR:` lines on the action's
behalf. This indirection is load-bearing: the body runs in a
command-substitution subshell, so a plain assignment cannot reach the caller —
without the re-emit the check would fail with no explanation at all.

Inline workflow bash has no subshell to escape, so it echoes the marker
directly:

```bash
run_action "Telnet blocked" bash -c '
    if nc -zw3 "$CRUCIBLE_TARGET_IP" 23 2>/dev/null; then
        echo "STUDENT_MSG: Port 23 is still open. Block it: sudo ufw deny 23/tcp"
        exit 1
    fi
'
```

The **last** `STUDENT_MSG:` line in an action's output becomes the message the
student sees. Everything else printed is instructor-only. Make the message
actionable: instead of "Failed", say "Open port 23. Run: `sudo ufw deny
23/tcp`". Students learn faster when the error names the next move.

---

## Platform tags — why they matter

`supported_platforms` does exactly one thing at run time: any action tagged
`windows` is **excluded** from the bash action library the runner sources
(`NOT (supported_platforms @> '["windows"]')` in
[`internal/engine/queries.go`](../../internal/engine/queries.go)). That cut is
there because a Windows body is PowerShell, and the library is sourced as a
single bash file — one unparseable body breaks every action in the run, not
just its own.

Everything else is advisory: nothing compares a Linux-only action against the
target's actual OS and aborts. A `linux:ubuntu` action pointed at a Debian
target will simply run. Tag honestly anyway — the picker groups on it, and it
is the only record of where an action has been proven.

| Tag | Matches |
|---|---|
| `any` | Any target |
| `linux` | Any Linux |
| `linux:ubuntu` | Ubuntu, any version |
| `linux:debian` | Debian-family, any version |
| `linux:rhel` | RHEL/CentOS/Rocky/Alma |
| `windows` | Any Windows |
| `windows:server2019` | A specific Windows variant |

Defined in
[`internal/database/migrations/000015_action_platforms.up.sql`](../../internal/database/migrations/000015_action_platforms.up.sql).

Start narrow (`linux:ubuntu`) and widen only after confirming the action works
on the broader set. A `linux` claim that breaks on Alpine is worse than a
missing claim.

Because of that filter, a `windows`-tagged action **cannot be called with
`run_action`** from a `kali_runner` workflow — it is not in the library at all.
Windows checks belong in a `vmware_tools` workflow, whose script runs inside
the guest and does not source the library.

---

## Context: passing data between actions

`ctx_set <key> <value>` writes to the workflow's context file. Later steps read
it as `CTX_<KEY>`, upper-cased with every non-alphanumeric character turned
into `_`:

```bash
# Action 1 (a library body):
ctx_set "discovered_port" "8443"

# Action 2, or plain workflow bash after action 1:
curl -s --max-time 5 "http://$CRUCIBLE_TARGET_IP:$CTX_DISCOVERED_PORT/"
```

`run_action` also writes two keys for every action automatically, prefixed by
the **label** (lower-cased, non-alphanumerics collapsed to `_`):

| Key | Value |
|---|---|
| `<prefix>.status` | The action's **exit code** — `0` on pass. Not `"ok"`/`"fail"`. |
| `<prefix>.body` | The action's entire captured output, newlines flattened to spaces, capped at 4096 bytes. |

So `run_action "Find port" ...` yields `CTX_FIND_PORT_STATUS` and
`CTX_FIND_PORT_BODY`.

If an action's output is valid JSON, the top-level keys `id`, `url`, `slug` and
`name` — and **only** those four — are also lifted into
`<prefix>.<field>`. For anything else, call `ctx_set` explicitly; echoing
`{"port": 8443}` and expecting `CTX_..._PORT` will not work.

> [!warning]
> Under the `set -uo pipefail` header every workflow uses, reading a
> `CTX_*` variable that was never written **aborts the whole workflow**. Only
> read a key you know an earlier action in the *same* workflow wrote, or guard
> it: `"${CTX_DISCOVERED_PORT:-}"`.

Context is **per-workflow**. Two workflows in the same playlist do not share
it, and neither do two runs.

Do not use `$(ctx_get …)` inside a command. `ctx_get` exists for reading within
a single action body; interpolating it into a command line is how prior output
gets a chance to inject.

---

## Accepted but inert

These fields are part of the API and the database, and are **never read at
run time**. They are documented here so nobody authors against them again.

| Field | Reality |
|---|---|
| `params` | Stored as JSONB and returned by the API. It is **not** turned into `PARAM_<FIELD>` environment variables — no such variable exists anywhere in the runner. Use `--flag value` arguments and declare them in `input_context`. |
| `student_fail_hint` | Stored, read back by CRUD, and never rendered or emitted. `{{ params.X }}` mustache substitution does not exist. Put the message in `LAST_STUDENT_MSG` instead. |
| `timeout_seconds` (per action) | Stored, but nothing sets the `ACTION_TIMEOUT` variable `run_action` reads, so **every** action gets the built-in 30-second ceiling. The *workflow's* `timeout_seconds` is honoured. |
| `points` / `penalty` | Only meaningful for `scoring_mode: "points"`, which nothing implements: no code path totals a score. The playlist API rejects that mode rather than accepting it and quietly grading pass/fail. |

If you need one of these to work, that is a feature request, not an authoring
workaround.

---

## What the API rejects

Save-time validation exists because the generated library is **one bash file
sourced by every runner pod**. A single bad entry does not produce one red
check — it breaks `source`, and every assessment in flight goes red with an
error that names `actions.sh` rather than your action. So these are `400`s at
authoring time, when you are there to read them:

| Rejected | Why |
|---|---|
| A slug that is not lowercase kebab-case | `HTTP-Get`, `http_get`, `-lead`, `trail-`, `a--b`, `2fast`. The runner function name is the slug with `-` replaced by `_`, and this shape is what keeps that mapping unambiguous. |
| A slug whose function name is already taken | Only reachable against a legacy row stored before the rule existed. The error names the other action. |
| A body that will not parse as bash | Checked as `slug_name() { your body }`, exactly how the engine renders it — so `local` and `return` are fine, an unclosed `if` is not. |

A `windows`-tagged action skips the bash check: its body is PowerShell, and it
is excluded from the bash library anyway.

---

## Where the library lives

The library is not "whatever happens to be in the database". Its in-repo source
of truth is [`deploy/sql/library-actions.sql`](../../deploy/sql/library-actions.sql),
with the ready-made assessments that call it in
[`deploy/sql/library-workflows.sql`](../../deploy/sql/library-workflows.sql).
Both are convergent: every entry is `ON CONFLICT (slug) DO UPDATE`, so
re-applying a file restores the committed definition rather than skipping it,
and a hand-edit made in the admin UI does not quietly become the new truth.

That matters because it is what makes the library reviewable. Before this
existed, the actions lived only in the production database, a fresh
environment came up with an empty library, and every workflow in it failed with
exit 127.

Add an action by editing that file (see the authoring rules in its header), then
run:

```bash
go test ./internal/libraryseed -update
```

That applies both files to a throwaway database twice, then checks each entry:
the slug is valid kebab-case, the body parses as bash when wrapped in its
generated function, every command it calls exists in
`runnertools/tools.txt` in `jmal1/selfservice-crucible-runner`, no two slugs generate the same function name,
and every `run_action` in every seeded workflow names a callable the seed
actually defines. `-update` also regenerates the two corpora
that `internal/engine`'s guards run against — without it those guards keep
passing while covering an older library.

Seeded workflows are written with `status='active'`, which bypasses the API's
activation gate. The gate is reproduced in that same test instead, so a
misspelled callable fails in CI rather than in a student's assessment.

---

## Lifecycle

Actions don't have a `draft → approved` lifecycle the way workflows do.
They're created directly and immediately callable. Edits replace the
existing entry — there's no per-revision pinning.

> [!warning]
> Because actions aren't versioned, **renaming the slug renames the runner
> function**, and every workflow still calling the old name dies with exit 127
> inside a student's assessment. The API refuses a rename while any workflow
> script still references the old function, and names the workflows. To retire
> an action, add the new slug, migrate the workflows to it, then delete the old
> one.

---

## See also

- [Building Workflows](workflows.md) — how to call actions
- [Runner Environment](runner-environment.md) — what env vars actions can use
- [Troubleshooting](troubleshooting.md) — common action failures
- [AGENTS.md](../../AGENTS.md) — full action schema
