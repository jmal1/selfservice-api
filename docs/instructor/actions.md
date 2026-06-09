# Building Library Actions

An **action** is a reusable, parameterised grading step. If you find
yourself copying the same `run_action "..." bash -c '...'` block into
three workflows, it's time to promote it to the library.

> [!note]
> If you haven't already, read [Building Workflows](workflows.md) first.
> Actions only make sense once you've felt the pain of repeating yourself.

---

## When to write an action (vs. inline bash)

| Situation | Use |
|---|---|
| One-off check, specific to a single assessment | **Inline** `run_action "<name>" bash -c '...'` |
| Same logic in 2+ workflows | **Library action** with parameters |
| Logic depends on context built up by earlier steps (e.g. "did the student install package X") | **Inline** — actions are stateless |
| You want students to see consistent error messages across labs | **Library action** with a `student_fail_hint` template |

> [!tip]
> Wait for the third copy-paste before extracting. Premature library
> growth becomes the next person's discovery problem.

---

## The shape of an action

Actions are created via `POST /api/v1/admin/actions`. Like workflows,
the admin UI is a thin wrapper.

```json
{
  "name": "Systemd Service Running",
  "slug": "service-running",
  "description": "Asserts the named systemd unit is active on the target.",
  "action_type": "command",
  "action_category": "system",
  "supported_platforms": ["linux:ubuntu", "linux:debian"],
  "params": {
    "service": {
      "type": "string",
      "required": true,
      "description": "systemd unit name (without .service)"
    }
  },
  "input_context": [],
  "output_context": ["service_running.status", "service_running.body"],
  "timeout_seconds": 15,
  "script": "ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 \"$CRUCIBLE_TARGET_USERNAME@$CRUCIBLE_TARGET_IP\" \"systemctl is-active --quiet ${PARAM_SERVICE}.service\"",
  "student_fail_hint": "Service {{ params.service }} is not running on your target. Start it with: sudo systemctl start {{ params.service }}"
}
```

### Field reference

| Field | Required | Notes |
|---|---|---|
| `name` | yes | Human-readable; shown in the action picker. |
| `slug` | yes | `[a-z0-9-]+`, globally unique. This is what workflows call. |
| `description` | yes | One sentence. Shown in the picker. |
| `action_type` | yes | `command` (most common), `query`, `mutation`, `assertion`. Hint for the UI. |
| `action_category` | yes | `system`, `network`, `security`, `forensics`, `recon`, `data`. |
| `supported_platforms` | yes | Array of `os:distro` slugs. The engine refuses to run if the target's platform isn't in this list. |
| `params` | no | Object of parameter schemas. Each is `{type, required, description, default?}`. |
| `input_context` | no | Names of context vars this action reads. Engine validates they're set. |
| `output_context` | no | Names of context vars this action writes. Always `<slug>.status` + optional `<slug>.body`. |
| `timeout_seconds` | no | Per-invocation timeout. Default 15. Keep small — actions are cheap. |
| `script` | yes | The bash body. Receives `PARAM_<NAME>` env vars + all standard runner env (see [Runner Environment](runner-environment.md)). |
| `student_fail_hint` | no | Mustache template (`{{ params.foo }}`) shown to students on failure. |

Full reference: [AGENTS.md](../../AGENTS.md) §3.2.

---

## Parameters: how they're passed in

Every entry in `params` becomes an env var inside the action script,
upper-cased and prefixed with `PARAM_`:

```json
"params": {
  "service": {"type": "string", "required": true},
  "expected_state": {"type": "string", "required": false, "default": "active"}
}
```

```bash
# Inside the script:
echo "Checking $PARAM_SERVICE is $PARAM_EXPECTED_STATE"
```

A workflow calls it like this:

```bash
run_action "service-running" service=ssh expected_state=active
```

> [!warning]
> Param values are passed through the shell unchanged. **Never `eval`**
> a param. If you need to interpolate into a command, quote it:
> `"systemctl is-active --quiet ${PARAM_SERVICE}.service"`.

---

## Platform tags — why they matter

`supported_platforms` is a safety net. If a workflow calls
`service-running` on a Windows target, the engine refuses to run it
rather than producing a confusing error.

Tags follow the pattern `<os>:<distro>` or `<os>:*`:

| Tag | Matches |
|---|---|
| `linux:ubuntu` | Ubuntu, any version |
| `linux:debian` | Debian, any version |
| `linux:*` | Any Linux |
| `windows:server2022` | Windows Server 2022 |
| `windows:*` | Any Windows |

Defined in
[`internal/database/migrations/000015_action_platforms.up.sql`](../../internal/database/migrations/000015_action_platforms.up.sql).

> [!tip]
> Start narrow (`linux:ubuntu`) and widen only when you've confirmed
> the action works on the broader set. A `linux:*` claim that breaks
> on Alpine is worse than a missing claim.

---

## Output context

Every `run_action` writes at least one context variable: `<slug>.status`
(`"ok"` on exit 0, `"fail"` otherwise). If your action also wants to
share data with later actions in the same workflow, declare it in
`output_context` and echo a JSON block:

```bash
# Inside an action that wants to share data:
result=$(ssh "$CRUCIBLE_TARGET_USERNAME@$CRUCIBLE_TARGET_IP" "ip -j a")
echo "::ctx::interfaces.body=$result"
```

Later actions can then `$CTX_INTERFACES_BODY`. See the engine source
in [`internal/engine/engine.go`](../../internal/engine/engine.go) for
the exact format.

> [!important]
> Output context is **per-workflow**, not global. Two different
> playlist runs see independent context; one workflow can't poison
> another's view.

---

## student_fail_hint

This is the single most underused feature. Define it once on the action
and every workflow that calls it gets the same helpful error message.

```
"student_fail_hint": "Port {{ params.port }} is open on your target. Block it with: sudo ufw deny {{ params.port }}/tcp"
```

Mustache-style `{{ params.foo }}` substitutions resolve at run time
using the actual param values.

> [!tip]
> Make the hint actionable. "Failed" → "Open port 23. Run: `sudo ufw
> deny 23/tcp`". Students learn faster when the error tells them the
> next move.

---

## Lifecycle

Actions don't have a `draft → approved` lifecycle the way workflows do.
They're created directly and immediately callable. Edits replace the
existing entry — there's no per-revision pinning.

> [!warning]
> Because actions aren't versioned, **renaming the slug breaks every
> workflow that depends on it**. If you must rename, add a new slug,
> migrate workflows to it, then delete the old one.

---

## See also

- [Building Workflows](workflows.md) — how to call actions
- [Runner Environment](runner-environment.md) — what env vars actions can use
- [Troubleshooting](troubleshooting.md) — common action failures
- [AGENTS.md](../../AGENTS.md) — full action schema
