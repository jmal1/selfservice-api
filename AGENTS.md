# AGENTS.md — Crucible AI Authoring Reference

> **Audience:** AI coding assistants (Copilot CLI, GitHub Copilot in VS Code, Claude Code, Codex CLI, Cursor, etc.) being asked to help instructors author Crucible **workflows**, **actions**, and **playlists**.
>
> **Promise:** Read this top-to-bottom and you have everything you need to produce working YAML/JSON for a Crucible workflow, an action library entry, or a playlist — *without* poking around the codebase or guessing schemas.
>
> **You are not the developer here.** You are helping a cybersecurity instructor write *content* (assessments) for a teaching platform called Crucible. Stay focused on workflows/actions/playlists. Don't refactor the engine, don't add new action types, don't change schemas.

---

## 1. What Crucible is, in two paragraphs

Crucible is a self-service VM lab for cybersecurity students. Instructors define **blueprints** (multi-VM pod recipes built from VM **templates**). Students click "Deploy" and get an isolated pod on its own VLAN with a runner Kali VM. Instructors author **automated assessments** that grade a student's pod (e.g. "did the student successfully harden SSH on their target VM?") by running scripts.

The grading layer has three nouns: **actions** (one shell-script step), **workflows** (an ordered list of actions, all running in one bash process), and **playlists** (ordered collection of workflows). When a student clicks "Run Assessment", the engine spins up a runner pod, executes the playlist's workflows against their pod, and stores pass/fail + per-action output in the database. Two execution modes exist: `kali_runner` (script runs on a Kali pod with network access to the target) and `vmware_tools` (script runs *inside* the target VM via VMware Tools — useful for "is service X running?" checks that need to be on-VM).

---

## 2. The two things you'll be asked to author

### 2a. A **workflow** = one assessment

Example user request: *"Write a workflow that checks the student blocked port 23 on their target."*

You produce a JSON payload for `POST /api/v1/admin/workflows`:

```json
{
  "name": "Telnet Blocked",
  "slug": "telnet-blocked",
  "description": "Asserts that TCP/23 on the target VM is closed or filtered.",
  "category": "hardening",
  "execution_mode": "kali_runner",
  "creation_mode": "script",
  "timeout_seconds": 60,
  "target_os": "linux",
  "script": "source /opt/crucible/lib/actions.sh\nset -euo pipefail\nrun_action \"port-23-closed\" bash -c 'if nc -zw3 \"$CRUCIBLE_TARGET_IP\" 23; then echo \"STUDENT_MSG: Telnet is still reachable on port 23. Block it with: sudo ufw deny 23/tcp\"; exit 1; fi'\n",
  "visible_to_students": true,
  "status": "draft"
}
```

That single API call is sufficient to create the workflow. After review, an admin moves it `draft` → `pending_review` → `approved` → `active` via the workflow lifecycle endpoints.

### 2b. A reusable **library action**

Example user request: *"Add a library action that checks if a systemd unit is running on the target."*

You produce a JSON payload for `POST /api/v1/admin/actions`:

```json
{
  "name": "Systemd Service Running",
  "slug": "service-running",
  "description": "Asserts the named systemd unit is active (running) on the target VM.",
  "action_type": "command",
  "action_category": "system",
  "supported_platforms": ["linux:ubuntu", "linux:debian"],
  "params": {
    "service": {"type": "string", "required": true, "description": "systemd unit name (without .service)"}
  },
  "input_context": [],
  "output_context": ["service_running.status", "service_running.body"],
  "timeout_seconds": 15,
  "script": "ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 \"$CRUCIBLE_TARGET_USERNAME@$CRUCIBLE_TARGET_IP\" \"systemctl is-active --quiet ${PARAM_SERVICE}.service\"",
  "student_fail_hint": "Service {{ params.service }} is not running on your target. Start it with: sudo systemctl start {{ params.service }}"
}
```

A workflow then references this action by including a `run_action "service-running" ...` invocation in its `script`.

---

## 3. Hard schema reference

All field types/values are enforced by Postgres CHECK constraints in [`internal/database/migrations/000012_assessment_engine.up.sql`](internal/database/migrations/000012_assessment_engine.up.sql) and the Go models in [`internal/models/workflow_models.go`](internal/models/workflow_models.go). **Do not invent new enum values — the API will 400.**

### 3.1 `Workflow`

| Field | Type | Allowed / Default | Notes |
|---|---|---|---|
| `name` | string | required | Human-readable title shown in admin UI |
| `slug` | string | required, globally unique | `[a-z0-9-]+`, used in URLs & runner config |
| `description` | string | default `""` | Markdown supported |
| `category` | string | default `"general"` | Freeform; common: `general`, `hardening`, `networking`, `forensics`, `recon` |
| `execution_mode` | enum | `"kali_runner"` (default) \| `"vmware_tools"` | See §4 |
| `creation_mode` | enum | `"visual"` (default) \| `"script"` | `visual` = built via UI builder; `script` = raw bash author |
| `script` | string | default `""` | The full bash to execute (start with `source /opt/crucible/lib/actions.sh`) |
| `setup_script` | string \| null | optional | Runs once before `script`; hard 60s timeout |
| `timeout_seconds` | int | default 300 | Hard ceiling for the entire workflow |
| `target_os` | string \| null | optional | Soft hint, e.g. `"linux"`, `"windows"` |
| `target_vm` | string \| null | optional | Slot name within the pod (default = primary target) |
| `guest_interpreter` | string \| null | optional | Only for `vmware_tools` mode: path inside guest, e.g. `/bin/bash`, `cmd.exe`, `powershell.exe` |
| `required_services` | string[] | optional | Engine-level preflight: e.g. `["vcenter", "nats"]` |
| `metadata` | jsonb | default `{}` | Free-form instructor notes |
| `visible_to_students` | bool | default `false` | Student-visible name + STUDENT_MSG only — instructor-only output stays hidden |
| `status` | enum | `"draft"` (default) \| `"pending_review"` \| `"approved"` \| `"active"` | Lifecycle gate; only `active` can be added to a playlist run |

### 3.2 `Action`

| Field | Type | Allowed / Default | Notes |
|---|---|---|---|
| `name` | string | required | Library: human title; in-workflow: action label |
| `slug` | string \| null | required for library actions | `[a-z0-9-]+`, globally unique |
| `description` | string | default `""` | What the action asserts |
| `action_type` | string | default `"command"` | Currently only `"command"` is supported by the runner. Reserved future: `"http"`, `"file_check"`, etc. |
| `action_category` | string | default `"general"` | Common: `network`, `system`, `auth`, `forensics`, `crypto`, `web` |
| `params` | jsonb | default `{}` | JSON-Schema-ish: `{ "fieldName": {"type": "string", "required": true, "description": "..."} }` |
| `script` | string | default `""` | The bash that gets run when the workflow invokes this action |
| `input_context` | jsonb (array) | default `[]` | List of `CTX_*` keys this action reads (for the editor's lint) |
| `output_context` | jsonb (array) | default `[]` | List of `CTX_*` keys this action will write |
| `timeout_seconds` | int | default 60 | Per-action ceiling |
| `student_fail_hint` | string \| null | optional | Shown to students on fail. Supports `{{ params.X }}` templating |
| `points` | int \| null | optional | Used when parent playlist is `scoring_mode=points` |
| `penalty` | int \| null | optional | Negative points awarded for failure |
| `is_library` | bool | server-set to `true` for `POST /admin/actions` | Standalone library entries have no `workflow_id` |
| `supported_platforms` | jsonb (array) | default `["any"]` | See §5 |

### 3.3 `Playlist`

| Field | Type | Default | Notes |
|---|---|---|---|
| `name`, `slug`, `description` | strings | required | |
| `scoring_mode` | enum | `"pass_fail"` (default) \| `"points"` | |
| `workflows` | array of `{workflow_id, execution_order}` | required ≥1 | Workflows execute in `execution_order` ASC |

Playlists attach to **templates** (default for any VM cloned from that template) via `POST /api/v1/admin/templates/{templateID}/playlists` or to **blueprint VM slots** (overrides template defaults for that slot) via `POST /api/v1/admin/blueprints/{blueprintID}/vm-playlists`.

### 3.4 `Run` and `WorkflowResult` (read-only for your purposes)

You will never `POST` runs yourself — they're created when a student clicks Run. But you may be asked to read run results from `GET /api/v1/admin/runs` or to interpret `WorkflowResult.action_results` JSON. Schema is in [`workflow_models.go`](internal/models/workflow_models.go) lines 163-207.

---

## 4. Execution modes — `kali_runner` vs `vmware_tools`

> **You almost always want `kali_runner`.** Pick `vmware_tools` only when the check fundamentally needs to be *inside* the target.

### 4.1 `kali_runner` (default)

- Engine launches a K8s Job (Kali Linux pod) on the pod's VLAN, with `CRUCIBLE_TARGET_IP` pointing at the student's VM.
- Your `script` runs inside the Kali pod and uses **outside-the-VM** tools: `nmap`, `nc`, `curl`, `ssh`, `nslookup`, `nikto`, etc.
- Network reachable as if you were a host on the same VLAN as the student.
- Has `bash`, `jq`, `socat`, `nmap`, `curl`, `git`, `ssh`, common pen-test tools.
- Use this for: port scans, HTTP probes, SSH-based command checks, DNS checks, anything you can test from a peer machine.

### 4.2 `vmware_tools`

- Engine uses **govmomi** to call `guest.OperationsManager.StartProgramInGuest` on the target VM via vCenter. Script literally runs inside the guest as root/Administrator.
- **Requires** `guest_interpreter` to be set (e.g. `/bin/bash`, `powershell.exe`, `cmd.exe`).
- Output is captured by writing to `/tmp/crucible-output-{runid}.json` inside the guest, then `InitiateFileTransferFromGuest` to pull it back.
- Slower (10-30s overhead per workflow vs. ~2s for `kali_runner`).
- VMware Tools must be installed and running in the guest — won't work on minimal/headless OSes without it.
- Use this for: "is service X running?", "does file Y exist?", "is firewall rule Z present?", anything where the answer is only knowable from inside the VM.

### 4.3 Script header conventions

Every `kali_runner` workflow `script` should start with:

```bash
source /opt/crucible/lib/actions.sh
set -euo pipefail
```

This loads `run_action`, `ctx_set`, `ctx_get` and gives you safe failure semantics.

`vmware_tools` scripts cannot source `/opt/crucible/lib/actions.sh` (the file doesn't exist inside the guest). Use plain bash/PowerShell and print `STUDENT_MSG:` lines for student-facing feedback. Exit non-zero on fail.

---

## 5. The `run_action` runtime contract

When the runner pod executes a workflow's `script`, it provides these helpers and conventions. **Authoring AIs MUST follow them or the result will be reported as `error`, not `fail`.**

### 5.1 Environment variables provided

| Variable | Description |
|---|---|
| `CRUCIBLE_TARGET_IP` | Primary target VM IP on the pod VLAN |
| `CRUCIBLE_TARGET_OS` | One of `linux`, `windows`, `other` |
| `CRUCIBLE_TARGET_USERNAME` | Default credentials for the target (set by template) |
| `CRUCIBLE_TARGET_PASSWORD` | Default credentials for the target |
| `CRUCIBLE_POD_SUBNET` | The pod's VLAN CIDR (e.g. `10.30.5.0/24`) |
| `CRUCIBLE_POD_INDEX` | Numeric index of the pod within its blueprint |
| `CRUCIBLE_WORKDIR` | A per-workflow scratch directory; deleted after run |
| `CRUCIBLE_SOCKET` | Unix socket the sidecar listens on (used by `run_action`) |
| `CRUCIBLE_CONTEXT` | Path to the per-workflow JSON context file (used by `ctx_set`/`ctx_get`) |
| `CTX_<PREFIX>_<FIELD>` | Sanitized values produced by previous actions (see §5.4) |
| `PARAM_<UPPER_FIELD>` | Action parameters injected from `params` (library actions only) |

### 5.2 `run_action "<label>" <command...>`

Wraps a single command, reports start/end events to the sidecar, captures exit code + duration, and auto-extracts JSON fields from stdout into the context.

```bash
# Single command, no shell interpolation
run_action "ssh-root-denied" ssh -o BatchMode=yes -o ConnectTimeout=5 root@"$CRUCIBLE_TARGET_IP" true

# Bash one-liner — wrap in bash -c
run_action "tcp-22-open" bash -c 'nc -zw3 "$CRUCIBLE_TARGET_IP" 22'

# Multi-step check
run_action "verify-firewall" bash -c '
  sudo -n true 2>/dev/null || { echo "STUDENT_MSG: ufw status requires sudo on the runner."; exit 2; }
  ufw status | grep -q "Status: active"
'
```

Exit codes the runner understands:
- `0` → action `pass`
- `124` → action `timeout` (from `timeout` command — leave this to the runner, don't catch it)
- any other → action `fail`

### 5.3 Student vs instructor output

- Print `STUDENT_MSG: <one-line message>` from stdout. The **last** such line in the action's output becomes the user-facing failure message. Use it for actionable remediation hints ("Service X is not running. Start with `sudo systemctl start X`").
- Everything else printed to stdout/stderr is captured as **instructor-only** output in `WorkflowResult.instructor_output`. Use it for debug info that students should never see (raw nmap output, full curl traces).
- For library actions, `student_fail_hint` field is shown on fail and supports `{{ params.X }}` templating; prefer this over `STUDENT_MSG` for reusable actions.

### 5.4 Context passing between actions

```bash
# Action 1: discovers something
run_action "find-web-port" bash -c '
  PORT=$(nmap -p 80,8080,8443 --open "$CRUCIBLE_TARGET_IP" -oG - | awk -F: "/Ports:/ {print \$3}" | grep -oP "^\d+" | head -1)
  echo "{\"port\": $PORT}"      # auto-extracted as find_web_port.port
'

# Action 2: uses the discovered value via CTX_<PREFIX>_<FIELD>
#  - the action label "find-web-port" becomes prefix "find_web_port"
#  - the JSON field "port" becomes CTX_FIND_WEB_PORT_PORT
run_action "fetch-banner" bash -c '
  curl -s --max-time 5 "http://$CRUCIBLE_TARGET_IP:$CTX_FIND_WEB_PORT_PORT/" | head -c 200
'
```

Rules:
- **NEVER** call `$(ctx_get foo)` in a command — `ctx_get` is for reading only inside the *same* action. Use the `CTX_*` env vars for cross-action reads. The sidecar sanitizes them; raw `ctx_get` does not, and a malicious previous output could shell-inject.
- Auto-extracted JSON fields from stdout: any top-level `id`, `url`, `slug`, `name` keys are pulled into context automatically. To capture other fields, call `ctx_set` explicitly.
- Context is per-workflow, not per-run. Two workflows in the same playlist do not share context.

---

## 6. Platform tagging — `supported_platforms`

Tells the engine + UI which target OSes an action works against. Format: JSON array of platform strings.

| Tag | Matches |
|---|---|
| `"any"` | Any target OS |
| `"linux"` | Any Linux distro |
| `"linux:ubuntu"` | Ubuntu specifically |
| `"linux:debian"` | Debian-family (incl. Ubuntu) |
| `"linux:rhel"` | RHEL/CentOS/Rocky/Alma |
| `"windows"` | Any Windows |
| `"windows:server2019"` | Specific Windows variant |

Examples:
- `["any"]` — pure network probe (e.g. `port-open`, `http-get`)
- `["linux"]` — needs an SSH connection (`ssh-exec`)
- `["linux:ubuntu", "linux:debian"]` — uses `ufw` or `apt`
- `["windows"]` — uses PowerShell over WinRM

When a workflow has `execution_mode = "vmware_tools"`, the engine cross-checks the target template's `os_type` against every referenced action's `supported_platforms`. A mismatch aborts the run with a clear error.

---

## 7. Action library catalog (curated snapshot)

> **Live source of truth:** `GET /api/v1/admin/actions` (returns the full current library). The list below is a representative slice maintained by hand for AI context; when in doubt, query the API.

| Slug | Category | Platforms | What it asserts |
|---|---|---|---|
| `http-get` | network | any | `curl -fsS <url>` returns 2xx |
| `http-post` | network | any | POST to URL succeeds with optional body match |
| `port-open` | network | any | `nc -zw3 <host> <port>` succeeds |
| `port-closed` | network | any | `nc -zw3 <host> <port>` fails (refused or filtered) |
| `dns-resolves` | network | any | `dig +short <name>` returns ≥1 record |
| `nmap-service` | network | any | `nmap -sV` reports specified service on port |
| `smb-share-accessible` | network | any | `smbclient -L //host` succeeds |
| `ssh-exec` | system | linux | `ssh user@host '<cmd>'` exits 0 |
| `ssh-denied` | auth | linux | `ssh user@host true` fails (auth denied) |
| `command-check` | system | linux | Run shell command on target via SSH, assert exit 0 |
| `wait-for` | system | linux | Poll a condition with backoff up to N seconds |
| `git-clone` | system | linux | Clone a repo into runner workdir |
| `git-push` | system | linux | Push from a runner-staged repo |
| `file-contains` | system | linux:ubuntu, linux:debian | Assert file on target contains regex (via SSH `grep`) |
| `service-running` | system | linux:ubuntu, linux:debian | `systemctl is-active --quiet <unit>` |
| `package-installed` | system | linux:ubuntu, linux:debian | `dpkg -l <pkg>` exit 0 |
| `ufw-enabled` | system | linux:ubuntu, linux:debian | `ufw status` reports active |
| `ufw-rule-exists` | system | linux:ubuntu, linux:debian | `ufw status numbered` contains the rule |

If you need an action that's not in the library, **prefer adding it as a library action** (via `POST /api/v1/admin/actions`) over inlining the logic in a workflow `script`. Library actions are reusable, tested once, and platform-tagged.

---

## 8. Bash conventions for authoring scripts

✅ **DO:**

- `source /opt/crucible/lib/actions.sh` and `set -euo pipefail` at the top of every `kali_runner` workflow.
- Quote *every* variable expansion: `"$CRUCIBLE_TARGET_IP"`, not `$CRUCIBLE_TARGET_IP`.
- Use `bash -c '...'` for any inline shell logic; this gives a clean per-action subshell.
- Use `timeout <n>` only if you need a sub-action timeout — `run_action` already enforces `ACTION_TIMEOUT` (default 30s).
- Print actionable `STUDENT_MSG:` hints for every fail path.
- Use `--max-time` on `curl`, `-w3` on `nc`, `-o ConnectTimeout=5` on `ssh` — never write a check that can hang forever.
- Exit `1` (or any non-zero) cleanly on fail; let `run_action` translate that to status `fail`.

❌ **DO NOT:**

- Run privileged operations the runner doesn't have rights for (`iptables`, raw socket, kernel module loads). The Kali pod is unprivileged.
- Hit the public internet (no `apt update`, no `curl github.com`). Only target the pod's VLAN.
- `sleep` more than 60s in any single action — use `wait-for` with explicit polling.
- Write to `$HOME` or `/tmp` outside `$CRUCIBLE_WORKDIR` — runner cleanup won't reach those files.
- Use `eval`, `$(ctx_get …)` interpolated into commands, or any pattern that lets prior output shell-inject. **Use `CTX_*` env vars** for cross-action data.
- Hardcode IPs/credentials — pull from `CRUCIBLE_TARGET_*` env vars.
- Catch exit code 124 in your bash — it's the timeout signal the runner needs to see.

---

## 9. Common workflow patterns

### 9.1 Port-state assertion

```bash
source /opt/crucible/lib/actions.sh
set -euo pipefail

run_action "port-23-closed" bash -c '
  if nc -zw3 "$CRUCIBLE_TARGET_IP" 23 2>/dev/null; then
    echo "STUDENT_MSG: Port 23 (telnet) is still open. Block it: sudo ufw deny 23/tcp"
    exit 1
  fi
'
```

### 9.2 Service-on-target via SSH

```bash
source /opt/crucible/lib/actions.sh
set -euo pipefail

SSH="ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -o BatchMode=no \
     -i /opt/crucible/keys/student_rsa"

run_action "nginx-running" bash -c "
  $SSH \"\$CRUCIBLE_TARGET_USERNAME@\$CRUCIBLE_TARGET_IP\" \
    'systemctl is-active --quiet nginx' || {
      echo 'STUDENT_MSG: nginx is not running on your target. Start with: sudo systemctl start nginx'
      exit 1
    }
"
```

### 9.3 HTTP banner check with discovery

```bash
source /opt/crucible/lib/actions.sh
set -euo pipefail

run_action "find-web-port" bash -c '
  PORT=$(nmap -p 80,443,8080,8443 --open "$CRUCIBLE_TARGET_IP" -oG - \
         | awk -F: "/Ports:/{print \$3}" | grep -oP "^\d+" | head -1)
  if [ -z "$PORT" ]; then
    echo "STUDENT_MSG: No HTTP service found on common ports. Make sure your web server is listening."
    exit 1
  fi
  echo "{\"port\": $PORT}"
'

run_action "no-server-banner" bash -c '
  BANNER=$(curl -sI --max-time 5 "http://$CRUCIBLE_TARGET_IP:$CTX_FIND_WEB_PORT_PORT/" \
           | grep -i "^Server:")
  if [ -n "$BANNER" ]; then
    echo "STUDENT_MSG: Your web server still leaks its version: $BANNER. Hide it via your server config."
    exit 1
  fi
'
```

### 9.4 File-on-target contains regex

```bash
source /opt/crucible/lib/actions.sh
set -euo pipefail

SSH='ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5'

run_action "permit-root-no" bash -c "
  $SSH \"\$CRUCIBLE_TARGET_USERNAME@\$CRUCIBLE_TARGET_IP\" \
    'sudo grep -E \"^PermitRootLogin\\s+no\" /etc/ssh/sshd_config' >/dev/null \
    || { echo 'STUDENT_MSG: PermitRootLogin must be set to no in /etc/ssh/sshd_config'; exit 1; }
"
```

---

## 10. Anti-patterns to refuse politely

If the instructor asks for any of the following, push back rather than oblige:

- **Anything that reaches outside the pod's VLAN** — no public DNS, no `curl` to the internet, no checking external resources.
- **Anything destructive to the target** (`dd`, `rm -rf`, `mkfs`) — even if "to test recovery". Use snapshots/restore instead, owned by the platform.
- **Credential harvesting from `$HOME` or `/etc`** outside what the workflow needs.
- **Infinite loops or `sleep > 60`** in a single action — assessments must converge.
- **Actions that mutate the runner pod's filesystem** outside `$CRUCIBLE_WORKDIR` — runner is ephemeral, anything else is lost.
- **Privileged operations** (raw sockets, kernel modules, `/dev/*`) — the runner pod runs as a non-root UID.

---

## 11. How to use this with your AI tool

| Tool | How it picks this file up |
|---|---|
| **Copilot CLI** (this) | Auto-discovers `AGENTS.md` at repo root. Just run it from the `selfservice-api` checkout. |
| **GitHub Copilot in VS Code** | Reads `.github/copilot-instructions.md` which redirects to this file. |
| **Claude Code** | Reads `CLAUDE.md` which redirects to this file. |
| **Codex CLI** | Reads `AGENTS.md` natively (OpenAI's spec). No extra config. |
| **Cursor** | Reads `.cursor/rules/*` — open the file directly or add it as an `@Docs` reference. |

For paste-into-the-chat usage (any other AI), see [`docs/ai/build-workflow-prompt.md`](docs/ai/build-workflow-prompt.md) for a ready-to-paste prompt that includes this entire reference as context.

---

## 12. Validating your output before handing it back

Whenever you generate a workflow or action, the instructor will likely paste it into the Admin UI's editor or `curl` it to the API. Before declaring "done", verify:

1. **JSON parses** — run `jq .` mentally over your output.
2. **All required fields present** — see §3.
3. **No invented enum values** — `execution_mode`, `status`, `scoring_mode` must match §3.
4. **`script` will pass shellcheck** — common pitfalls:
   - Unquoted `$VAR` expansions (`SC2086`)
   - `[ $a = $b ]` with unquoted vars (`SC2086`)
   - `cd $dir; rm -rf …` without `cd … || exit` (`SC2164`)
   - `local x=$(…)` without checking exit code (`SC2155`)
5. **Slug uniqueness** — slugs are global. Pick something distinctive (`hardening-ssh-rootlogin-no`, not `ssh-check`).
6. **Timeouts are sane** — `timeout_seconds` should be 2-3× the slowest realistic action; default 60s for actions, 300s for workflows is usually right.
7. **No leaked secrets** — never echo `$CRUCIBLE_TARGET_PASSWORD` or write it to a file outside `$CRUCIBLE_WORKDIR`.

### 12.1 Admin script validator wrapper model (read before authoring action bodies)

The Admin UI's script editor sends your action body to `POST /api/v1/admin/scripts/validate`, which **wraps your script** in a synthetic preamble before running `shellcheck`. This is why some patterns that would look like errors in a standalone script don't get flagged, and vice versa:

| Pattern in your action body | Validator behaviour | Why |
|---|---|---|
| Top-level `local foo=bar` | **No SC2168.** Allowed. | The wrapper places your body inside a function shell (`_crucible_action_body() { … }`). |
| `LAST_ERROR="msg"` / `LAST_STUDENT_MSG="…"` set but seemingly unused | **No SC2034.** Allowed. | The wrapper consumes these after your body returns. |
| `$CTX_FOO` where `foo` was declared in **Input Context** on the action form | **No SC2154.** Allowed. | The wrapper emits `: "${CTX_FOO:=}"` for every declared input. |
| `$CTX_TYPO` where `typo` was **not** declared | **Not surfaced by shellcheck** (silent passthrough). | shellcheck's SC2154 heuristic intentionally ignores ALL_CAPS variables (they're assumed to be from the environment). **You are responsible for typo-checking your `$CTX_*` references.** A future validator pass will catch these statically. |
| `$x` in `[ -f $x ]` (unquoted) | **SC2086 fires.** Real bug. | Word-splitting + globbing risk. |
| Genuinely unused `local thisIsUnused=…` | **SC2034 fires.** Real bug. | We don't over-suppress. |
| `if [ -f /x ]` *without* `then` | **Parse error.** Real bug. | Will also be caught by the client-side `sh-syntax` layer instantly. |
| Body that looks like PowerShell (`$Var = …`, `Get-Service`, `param(…)`) | **Skipped, returns empty findings.** | Detected by shebang or heuristic; bash shellcheck would just produce noise. |

**Practical takeaway for authoring:** declare every context input you read on the action form (Input Context tab), use the `LAST_*` exit vars for error reporting, and don't bother adding `# shellcheck disable=…` comments — the wrapper has already handled the false positives that are specific to Crucible's runtime, and anything that does fire is almost certainly a real bug worth fixing.

The validator runs in two layers:
- **Client-side (300 ms debounce):** `sh-syntax` WASM in the browser catches syntax-level errors instantly with zero server load.
- **Server-side (2000 ms debounce + on Save):** real `shellcheck` runs in the API pod for the full SC**** rule library. Admin-only, 64 KB cap, 30 req/min rate limit, 3 s timeout.

Source: [`internal/scriptvalidator/wrap.go`](internal/scriptvalidator/wrap.go), [`internal/scriptvalidator/detect.go`](internal/scriptvalidator/detect.go), [`internal/scriptvalidator/validator.go`](internal/scriptvalidator/validator.go).

---

## 13. When you should hand back to the human

- The user asks for a check that requires a credential or tool not in the Kali runner. Recommend they request the addition or use `vmware_tools` mode.
- The user asks for a workflow that would mutate the student's VM in a way that breaks subsequent assessments. Suggest a snapshot/revert design instead.
- The user asks you to bypass `status=approved` gating. That's an instructor-admin decision, not an authoring decision.

---

## 14. Where the source of truth lives (for your reference, do not modify)

- Workflow/Action/Playlist Go models: [`internal/models/workflow_models.go`](internal/models/workflow_models.go)
- DB schema (the actual CHECK constraints): [`internal/database/migrations/000012_assessment_engine.up.sql`](internal/database/migrations/000012_assessment_engine.up.sql), [`000014_action_library.up.sql`](internal/database/migrations/000014_action_library.up.sql), [`000015_action_platforms.up.sql`](internal/database/migrations/000015_action_platforms.up.sql)
- Runner script library (the `run_action` / `ctx_set` source): [`deploy/runner/actions.sh`](deploy/runner/actions.sh)
- Runner executor (how scripts are launched): [`internal/runner/executor.go`](internal/runner/executor.go)
- Engine (how runs are orchestrated): [`internal/engine/engine.go`](internal/engine/engine.go)
- API routes: [`internal/api/routes/routes.go`](internal/api/routes/routes.go) — workflow + action endpoints under `/api/v1/admin/`
- Live action catalog (the *real* source of truth for available actions): `GET /api/v1/admin/actions` against your Crucible instance.
