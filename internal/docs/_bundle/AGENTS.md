# AGENTS.md — Crucible AI Authoring Reference

> **Audience:** AI coding assistants (Copilot CLI, GitHub Copilot in VS Code, Claude Code, Codex CLI, Cursor, etc.) being asked to help instructors author Crucible **workflows**, **actions**, and **playlists**.
>
> **Promise:** Read this top-to-bottom and you have everything you need to produce working YAML/JSON for a Crucible workflow, an action library entry, or a playlist — *without* poking around the codebase or guessing schemas.
>
> **You are not the developer here.** You are helping a cybersecurity instructor write *content* (assessments) for a teaching platform called Crucible. Stay focused on workflows/actions/playlists. Don't refactor the engine, don't add new action types, don't change schemas.

---

## 1. What Crucible is, in two paragraphs

Crucible is a self-service VM lab for cybersecurity students. Instructors define **blueprints** (multi-VM pod recipes built from VM **templates**). Students click "Deploy" and get an isolated pod on its own VLAN. Instructors author **automated assessments** that grade a student's pod (e.g. "did the student successfully harden SSH on their target VM?") by running scripts. Grading scripts execute in an ephemeral **Kali runner container** that the engine attaches to the pod's VLAN for the duration of a run — students are not given Kali VMs and cannot log into the runner.

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
  "script": "source /opt/crucible/lib/actions.sh\nset -uo pipefail\nrun_action \"port-23-closed\" bash -c 'if nc -zw3 \"$CRUCIBLE_TARGET_IP\" 23; then echo \"STUDENT_MSG: Telnet is still reachable on port 23. Block it with: sudo ufw deny 23/tcp\"; exit 1; fi'\n",
  "visible_to_students": true,
  "status": "draft"
}
```

That single API call is sufficient to create the workflow. After review, an admin moves it `draft` → `pending_review` → `approved` → `active` via the workflow lifecycle endpoints.

Editing a `pending_review`, `approved`, or `active` workflow returns it to
`draft` and clears its prior approval. New playlist runs exclude it until it is
reviewed and activated again; a run already in flight keeps its immutable
launch snapshot.

### 2b. A reusable **library action**

Example user request: *"Add a library action that checks if a systemd unit is running on the target."*

You produce a JSON payload for `POST /api/v1/admin/actions`:

```json
{
  "name": "Systemd Service Running",
  "slug": "service-running",
  "description": "Asserts the named systemd unit is active (running).",
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

That is the real shipped body, and it shows the three conventions every library
action follows:

1. **Arguments are `--flag value` pairs** parsed by a `while` loop. There is no
   `params` injection — see §3.2.
2. **`ctx_set`** publishes anything a later step may need.
3. **`LAST_ERROR` / `LAST_STUDENT_MSG` plus a non-zero `return`** report
   failure. `run_action` re-emits both on the action's behalf.

Declare each accepted flag in `input_context` so the editor and the script
validator know about it. The declaration does not create the variable; the
parse loop does.

A workflow references this action with a display label followed by the generated
snake-case callable: `run_action "Service is running" service_running --name ssh`.
The library slug remains `service-running`; do not pass that kebab-case slug as
the command.

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
| `action_type` | string | default `"command"` | **Labelling only — the runner ignores it.** No CHECK constraint. Production uses `command`, `dns`, `http`, `port_check`, `service_check`, `file_check`, `ssh`. |
| `action_category` | string | default `"general"` | **Labelling only.** No CHECK constraint. Production uses `general`, `network`, `file`, `service`, `ssh`, `firewall`. |
| `params` | jsonb | default `{}` | **Accepted and stored, but inert.** Nothing injects `PARAM_*` env vars. Real actions take `--flag value` arguments; leave this `{}`. |
| `script` | string | default `""` | The bash body. Rendered as a shell function, so it uses `local` and `return`, not `exit`. |
| `input_context` | jsonb (array) | default `[]` | Array of `{"key","type","description"}` objects. Declares the `--flag` names the body parses and any `CTX_*` it reads. |
| `output_context` | jsonb (array) | default `[]` | Array of `{"key","type","description"}` objects naming the `ctx_set` keys the body writes. |
| `timeout_seconds` | int | default 60 | **Inert for library actions.** `run_action` applies the workflow-level `ACTION_TIMEOUT` (default 30s) to every action. |
| `student_fail_hint` | string \| null | optional | **Accepted and stored, but inert.** Nothing renders it and `{{ params.X }}` is never templated. Assign `LAST_STUDENT_MSG` in the body instead. |
| `points` | int \| null | optional | Reserved: `scoring_mode=points` is not currently reachable (playlists are created `pass_fail`). |
| `penalty` | int \| null | optional | Reserved, same as `points`. |
| `is_library` | bool | server-set to `true` for `POST /admin/actions` | Standalone library entries have no `workflow_id` |
| `supported_platforms` | jsonb (array) | default `["any"]` | See §6. Only `windows` changes behaviour: it excludes the action from the bash library. |

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
set -uo pipefail
```

This loads `run_action`, `ctx_set`, `ctx_get`, and the generated **action library** (see §5.2b).

**Note `-uo`, not `-euo`.** `run_action` returns the action's exit code, so under `set -e` the first failing check terminates the workflow's single bash process. Every action after it never runs and never reports, and a student hardening six settings sees one red check followed by silence rather than six results — with nothing in the output that looks like an abort. The run just has fewer results than the workflow has actions. `set -u` and `set -o pipefail` are still wanted; only `-e` is harmful here. Pinned by `TestActionsSh_WorkflowSetEStopsAtTheFirstFailedCheck` in [`internal/runner/actions_sh_test.go`](internal/runner/actions_sh_test.go).

Inside an *action* body the situation is different and already handled: `run_action` runs the body under `set +e` so it can detect its own failure and set `LAST_STUDENT_MSG` before returning.

`vmware_tools` scripts cannot source `/opt/crucible/lib/actions.sh` (the file doesn't exist inside the guest). Use plain bash/PowerShell and print `STUDENT_MSG:` lines for student-facing feedback. Exit non-zero on fail.

---

## 5. The `run_action` runtime contract

When the runner pod executes a workflow's `script`, it provides these helpers and conventions. **Authoring AIs MUST follow them or the result will be reported as `error`, not `fail`.**

### 5.1 Environment variables provided

| Variable | Description |
|---|---|
| `CRUCIBLE_TARGET_IP` | Primary target VM IP on the pod VLAN. See "Which VM is the target?" below. |
| `CRUCIBLE_TARGET_OS` | One of `linux`, `windows`, `other` |
| `CRUCIBLE_TARGET_USERNAME` | Default credentials for the target (set by template) |
| `CRUCIBLE_TARGET_PASSWORD` | Default credentials for the target |
| `CRUCIBLE_POD_SUBNET` | The pod's VLAN CIDR (e.g. `10.30.5.0/24`) |
| `CRUCIBLE_POD_INDEX` | The pod's VLAN tag (e.g. `119`) — a stable numeric identifier unique to the pod's network. Previously documented as an index within a blueprint; the `pods.pod_index` column backing that was dropped in migration 000003, so the VLAN tag is now the pod's numeric identity. |
| `CRUCIBLE_WORKDIR` | A per-workflow scratch directory; deleted after run |
| `CRUCIBLE_SOCKET` | Unix socket the sidecar listens on (used by `run_action`) |
| `CRUCIBLE_CONTEXT` | Path to the per-workflow JSON context file (used by `ctx_set`/`ctx_get`) |
| `CTX_<KEY>` | Sanitized values produced by previous actions (see §5.4) |

**Which VM is the target?**

A workflow run grades **exactly one** VM: the `target_pod_vm_id` chosen when the
student (or API client) starts the assessment. The testing dashboard lists
playlists **per reachable pod VM**, so two VMs cloned from the same template
appear as separate assessment targets even though they share the same playlist
set. `POST /pods/{id}/testing/run` requires both `playlist_id` and
`target_pod_vm_id`; the engine fails closed if a run has no target.

The VM that was assessed is recorded on the run (`target_vm_name` /
`target_vm_ip`) and shown as **Assessed VM** / **Target VM** on the run detail
page (including on each workflow/check row), so you can always confirm what a
given result refers to.

### 5.2 `run_action "<label>" <command...>`

Wraps a single command, reports start/end events to the sidecar, captures exit code + duration, and auto-extracts JSON fields from stdout into the context.

Both parts are required: the first argument is only the human-readable result
label, and the second argument is the command or library function to execute.
`run_action "demo-http-service-reachable"` has no command and is invalid.

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

### 5.2b Calling a library action

Library actions (`is_library: true`) are delivered to the runner as **bash functions** and can be
called directly by `run_action`. The function name is the action's **slug with hyphens replaced by
underscores**:

| Slug | Function to call |
|---|---|
| `http-get` | `http_get` |
| `port-open` | `port_open` |
| `service-running` | `service_running` |
| `ufw-rule-exists` | `ufw_rule_exists` |

```bash
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "HTTPS responds" http_get --url "https://$CRUCIBLE_TARGET_IP/" --expect-status 200
run_action "SSH is open"    port_open --host "$CRUCIBLE_TARGET_IP" --port 22
```

The label and callable are deliberately different fields. For a library action
whose slug is `demo-http-service-reachable`, write:

```bash
run_action "DEMO - HTTP Service Reachable" demo_http_service_reachable
```

Do **not** write either malformed form:

```bash
run_action "demo-http-service-reachable"
run_action "DEMO - HTTP Service Reachable" demo-http-service-reachable
```

Workflow activation validates this contract against the current database action
library. A missing command or a known kebab-case library slug blocks activation
and the API response names the expected snake-case callable.

Pass the flags the action's own body parses — each library body has a `--flag value` argument loop;
read the action in the library catalog (`docs/instructor/actions.md`) to see its accepted flags.

How this works, and why it matters if you are changing the runner: the engine renders every
non-Windows library action into `/opt/crucible/lib/library.sh` **per run** and `actions.sh` sources
it. `run_action` detects a shell function with `declare -F` and re-enters bash inside the `timeout`
so the function is callable — a plain `timeout http_get …` cannot work, because `timeout` `execve()`s
its argument and a shell function is not an executable file. Windows library actions are excluded
because their bodies are PowerShell and the library is sourced as a single bash file.

Library bodies report problems by assigning `LAST_STUDENT_MSG` and `LAST_ERROR`; `run_action` emits
those as `STUDENT_MSG:` / `ERROR:` lines on the action's behalf, so §5.3 applies unchanged.

### 5.3 Student vs instructor output

- Print `STUDENT_MSG: <one-line message>` from stdout. The **last** such line in the action's output becomes the user-facing failure message. Use it for actionable remediation hints ("Service X is not running. Start with `sudo systemctl start X`").
- The message is taken **verbatim** after trimming surrounding whitespace. Quotes, backslashes and Windows paths survive intact, so `STUDENT_MSG: Expected "200" but got C:\inetpub` reaches the student exactly as written. Only leading/trailing whitespace is removed; nothing else is re-quoted or word-split.
- Everything else printed to stdout/stderr is captured as **instructor-only** output in `WorkflowResult.instructor_output`. Use it for debug info that students should never see (raw nmap output, full curl traces).
- Inside a **library action** body, assign `LAST_STUDENT_MSG` instead of echoing. `run_action` emits it as a `STUDENT_MSG:` line for you. The `student_fail_hint` column is stored but never rendered — do not rely on it.

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
- **NEVER** call `$(ctx_get foo)` in a command — `ctx_get` is for reading only inside the *same* action. Use the `CTX_*` env vars for cross-action reads.
- The env name is the context key upper-cased with every non-alphanumeric character replaced by `_`, prefixed with `CTX_`. So `ctx_set "web_port" 8080` in a library action body is readable as `$CTX_WEB_PORT` by whatever runs next.
- Auto-extracted JSON fields from stdout: any top-level `id`, `url`, `slug`, `name` keys are pulled into context automatically. To capture other fields, call `ctx_set` explicitly.
- The export happens at the end of each `run_action`, so a value is visible to the next action and to plain shell between actions — never to the action that wrote it.
- Values are flattened to one line and capped at 4096 bytes, matching what the sidecar would have produced.
- Context is per-workflow, not per-run. Two workflows in the same playlist do not share context.

---

## 6. Platform tagging — `supported_platforms`

Declares which target OSes an action works against. Format: JSON array of platform strings.

**Only one tag changes runtime behaviour: `windows`.** An action tagged
`["windows"]` is excluded from the generated bash library, because its body is
PowerShell and the library is sourced as a single bash file — one unparseable
body would take down every action in the run. Every other tag is documentation
for the instructor and the admin UI; nothing filters or aborts on it.

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
- `["windows"]` — PowerShell body, for a `vmware_tools` workflow only

A `windows`-tagged action is not callable with `run_action`, since it never
reaches the runner's library. Tagging an otherwise-fine bash action `windows`
silently makes it unavailable; that is the one tagging mistake with teeth.

---

## 7. Action library catalog

> **In-repo source of truth:** [`deploy/sql/library-actions.sql`](deploy/sql/library-actions.sql)
> — 65 library actions, each with its real bash body, convergent on slug.
> Read a body there when you need its exact flags. `GET /api/v1/admin/actions`
> returns what a given instance currently holds.
>
> The full-metadata mirror used by the corpus tests is
> [`internal/scriptvalidator/testdata/actions.json`](internal/scriptvalidator/testdata/actions.json);
> it is **generated** from the SQL by `go test ./internal/libraryseed -update`,
> so do not hand-edit it.

Callables are the slug with hyphens replaced by underscores. Flags listed are
exactly the ones each body's argument loop parses; anything else is ignored.
Where a flag is omitted, most actions default `--host` to
`$CRUCIBLE_TARGET_IP`.

**Anything that inspects the student's machine reaches it over SSH** using the
`crucible_ssh` / `crucible_ssh_sudo` helpers in
[`deploy/runner/actions.sh`](deploy/runner/actions.sh). Calling `systemctl`,
`dpkg` or `ufw` directly in an action body inspects the unprivileged Kali
runner pod instead — the defect that made `service_running --name ssh` grade
the runner's own sshd, and made `ufw_enabled` unable to pass at all.

### Network and recon (run from the runner)

| Slug | Type / Category | Platforms | Flags | What it asserts |
|---|---|---|---|---|
| `http-get` | http / network | any | `--url --expect-status --expect-body --timeout --max-time --cookies --save-cookies` | HTTP GET returns the expected status and body |
| `http-post` | http / network | any | `--url --data --json --expect-status --cookies --save-cookies` | HTTP POST returns the expected status |
| `http-header-present` | http / network | any | `--url --header --value` | Response sends a header, optionally matching a regex |
| `http-header-absent` | http / network | any | `--url --header --value` | Response does **not** send a header (version leaks) |
| `http-redirects-to-https` | http / network | any | `--host --path` | Plain HTTP answers 30x with an `https://` Location |
| `http-auth-required` | http / web | any | `--url` | Unauthenticated request is refused 401/403 |
| `http-credentials-rejected` | http / web | any | `--url --user --pass` | A default credential pair is **not** accepted |
| `directory-listing-disabled` | http / web | any | `--url` | URL returns no auto-generated directory index |
| `web-technology-hidden` | http / web | any | `--url --deny` | `whatweb` fingerprint advertises no version |
| `port-open` | port_check / network | any | `--host --port --timeout` | TCP port accepts a connection |
| `port-closed` | port_check / firewall | any | `--host --port` | TCP port is refused or filtered |
| `nmap-port-state` | port_check / network | any | `--host --port --state` | nmap reports open / closed / filtered |
| `nmap-only-expected-ports` | port_check / firewall | any | `--host --allow --range` | Nothing outside an allowlist is open |
| `nmap-service` | command / network | any | `--host --port --expect-service` | `nmap -sV` reports the expected service |
| `nmap-script-output` | command / network | any | `--host --port --script --expect --absent` | An NSE script's output matches a regex |
| `tcp-banner-matches` | port_check / network | any | `--host --port --expect --absent` | A service's connect banner matches a regex |
| `host-responds-to-ping` | command / network | any | `--host --absent` | Host answers (or deliberately ignores) ICMP echo |
| `dns-resolves` | dns / network | any | `--hostname --server --expect-ip` | Name resolves, optionally to an expected address |
| `dns-server-refuses-recursion` | dns / network | any | `--host --probe` | Server does not advertise recursion to any client |
| `tls-certificate-valid` | command / network | any | `--host --port --days` | Served certificate is valid and not near expiry |
| `tls-protocol-refused` | command / network | any | `--host --port --protocol` | An obsolete TLS/SSL version is refused |
| `smb-share-accessible` | command / network | any | `--host --share --user --pass` | SMB share is reachable |
| `smb-share-listable` | command / network | any | `--host --expect-share --absent` | Anonymous share listing works, or is refused |
| `smb-signing-required` | command / network | any | `--host` | SMB requires message signing (blocks relay) |
| `ftp-anonymous-denied` | command / network | any | `--host --port` | FTP refuses the anonymous account |
| `ldap-anonymous-bind-denied` | command / network | any | `--host --base` | Directory returns no entries to an anonymous bind |
| `redis-auth-required` | command / network | any | `--host --port` | Redis rejects unauthenticated commands |

### Runner-local utilities

| Slug | Type / Category | Platforms | Flags | What it asserts |
|---|---|---|---|---|
| `command-check` | command / general | linux | `--cmd --expect-exit --expect-output` | Command run **on the runner** exits/prints as expected |
| `wait-for` | command / general | linux | `--cmd --retries --delay --description` | Polls a condition until it passes |
| `git-clone` | command / general | linux | `--url --dest` | Clones a repo into the runner workdir |
| `git-push` | command / general | linux | `--repo --branch --file --content --message` | Commits and pushes from a staged repo |

### Target-side: SSH and services

| Slug | Type / Category | Platforms | Flags | What it asserts |
|---|---|---|---|---|
| `ssh-exec` | ssh / ssh | linux | `--host --user --key --command --expect-exit --expect-output` | Command over SSH exits/prints as expected |
| `ssh-denied` | ssh / ssh | linux | `--host --user` | SSH auth is refused |
| `ssh-key-auth-only` | ssh / ssh | linux | `--host --user` | sshd refuses password auth, proven from the network |
| `sshd-directive` | ssh / ssh | linux | `--directive --value` | An `sshd -T` effective directive has a value |
| `sshd-protocol-hardened` | ssh / ssh | linux | *(none)* | Root login and password auth are both off |
| `service-running` | service_check / service | linux | `--name --quiet` | systemd unit is active on the target |
| `service-enabled` | service_check / service | linux | `--name` | systemd unit is enabled at boot |
| `service-stopped` | service_check / service | linux | `--name` | systemd unit is **not** running |
| `service-listening-on` | command / network | linux | `--port --address` | Port is bound to an expected address, not `0.0.0.0` |
| `package-installed` | service_check / service | linux | `--name` | Package is installed on the target |
| `package-absent` | service_check / service | linux | `--name` | Package has been removed from the target |
| `unattended-upgrades-enabled` | service_check / service | linux:ubuntu, linux:debian | *(none)* | Automatic security updates installed **and** switched on |

### Target-side: hardening, accounts and files

| Slug | Type / Category | Platforms | Flags | What it asserts |
|---|---|---|---|---|
| `ufw-enabled` | service_check / firewall | linux | *(none)* | `ufw status` reports active on the target |
| `ufw-rule-exists` | service_check / firewall | linux | `--rule` | `ufw status numbered` contains the rule |
| `ufw-default-deny-incoming` | service_check / firewall | linux | *(none)* | Default inbound policy is deny |
| `file-contains` | file_check / file | linux | `--path --regex --value` | File on the target matches a regex or exact value |
| `file-permissions` | file_check / file | linux | `--path --mode --at-most` | Path has an exact mode, or grants no more than one |
| `file-owner` | file_check / file | linux | `--path --owner --group` | Path is owned by an expected user/group |
| `no-world-writable` | file_check / file | linux | `--path --max-depth` | No world-writable regular files in a tree |
| `user-exists` | command / system | linux | `--name` | A local account is present |
| `user-absent` | command / system | linux | `--name` | A local account has been removed |
| `user-in-group` | command / system | linux | `--user --group --absent` | Account is (or is not) a group member |
| `account-locked` | command / system | linux | `--name` | Account cannot authenticate with a password |
| `no-empty-passwords` | command / system | linux | *(none)* | No account in `/etc/shadow` has an empty password |
| `no-extra-uid-zero` | command / system | linux | *(none)* | root is the only UID 0 account |
| `no-passwordless-sudo` | command / system | linux | `--allow` | sudoers grants nobody unexpected NOPASSWD root |
| `sysctl-value` | command / system | linux | `--key --value` | A **running** kernel parameter has a value |
| `cron-entry-absent` | command / system | linux | `--pattern` | No crontab matches a pattern (scheduled persistence) |

### Windows (`vmware_tools` only)

`win-command-check`, `win-file-contains`, `win-firewall-enabled`,
`win-firewall-rule-exists`, `win-package-installed`, `win-service-running`.

Their bodies are PowerShell, so they are excluded from the bash library and are
**not** callable with `run_action`; see §6.

### Adding one

Prefer a new library action over inlining logic in a workflow `script`:
reusable, checked once, platform-tagged. Add it to
[`deploy/sql/library-actions.sql`](deploy/sql/library-actions.sql) (its header
has the authoring rules) and run `go test ./internal/libraryseed -update`. That
verifies the slug, the bash body, tool availability and callable uniqueness,
and regenerates both corpora. `POST /api/v1/admin/actions` applies the same
validation for an ad-hoc addition, but an action created that way exists only
in that one database.

### Ready-made workflows

[`deploy/sql/library-workflows.sql`](deploy/sql/library-workflows.sql) seeds 17
assignable assessments across four areas — Linux hardening (SSH baseline, host
firewall, account hygiene, kernel network settings, patching and persistence),
recon (attack surface, service banners, SMB enumeration, DNS posture), web
security (TLS configuration, security headers, access control, file hygiene),
and service configuration (web stack, database exposure, file sharing, account
provisioning). Each is `active` and calls only library actions.

---

## 8. Bash conventions for authoring scripts

✅ **DO:**

- `source /opt/crucible/lib/actions.sh` and `set -uo pipefail` at the top of every `kali_runner` workflow. Not `-euo`: see §4.3 — `set -e` makes the first failing check silently discard every check after it.
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
set -uo pipefail

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
set -uo pipefail

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
set -uo pipefail

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
set -uo pipefail

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
8. **Every `run_action` has label + callable** — for a library slug such as `port-open`, the second argument is `port_open`, not `port-open`.

### 12.0 What the create/update API rejects outright

These are `400`s (or `409` for the rename guard), enforced because the
generated library is one bash file sourced by every runner pod: a single bad
entry breaks `source` and fails every assessment in flight, not just its own.

| Endpoint | Rejected |
|---|---|
| `POST /admin/actions` | Slug that is not lowercase kebab-case `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`; a slug whose generated function collides with an existing action; a body that does not parse as bash when wrapped in `slug_name() { … }`. |
| `PATCH /admin/actions/{id}` | The same three, validated against the *merged* row — a new script is checked against the stored slug. Plus `409` on a slug rename while any workflow script still calls the old function; the response names the workflows. |
| `POST /admin/workflows` | Slug that is not kebab-case; an `execution_mode` or `creation_mode` outside the two legal values. |
| `POST /admin/playlists` | Slug that is not kebab-case; `scoring_mode: "points"`, which is unimplemented — nothing totals `points`/`penalty`, so accepting it would return `201` and then silently grade pass/fail. |

A `windows`-tagged action skips the bash parse, since its body is PowerShell
and it is excluded from the bash library.

### 12.1 Admin script validator wrapper model (read before authoring action bodies)

The Admin UI's script editor sends your action body to `POST /api/v1/admin/scripts/validate`, which **wraps your script** in a synthetic preamble before running `shellcheck`. This is why some patterns that would look like errors in a standalone script don't get flagged, and vice versa:

| Pattern in your action body | Validator behaviour | Why |
|---|---|---|
| Top-level `local foo=bar` | **No SC2168.** Allowed. | The wrapper places your body inside a function shell (`_crucible_action_body() { … }`). |
| `LAST_ERROR="msg"` / `LAST_STUDENT_MSG="…"` set but seemingly unused | **No SC2034.** Allowed. | The wrapper consumes these after your body returns. |
| `$CTX_FOO` where `foo` was declared in **Input Context** on the action form | **No SC2154 / no CRU0001.** Allowed. | The wrapper emits `: "${CTX_FOO:=}"` for every declared input. |
| `$CTX_TYPO` where `typo` was **not** declared | **CRU0001 fires** (warning). | shellcheck's SC2154 deliberately ignores ALL_CAPS variables, so a Crucible-specific static pass (`internal/scriptvalidator/ctxcheck.go`) catches undeclared `$CTX_*` refs. Emitted as a warning so legitimate runtime-injected names aren't blocked. |
| `$x` in `[ -f $x ]` (unquoted) | **SC2086 fires.** Real bug. | Word-splitting + globbing risk. |
| Genuinely unused `local thisIsUnused=…` | **SC2034 fires.** Real bug. | We don't over-suppress. |
| `if [ -f /x ]` *without* `then` | **Parse error.** Real bug. | Will also be caught by the client-side `sh-syntax` layer instantly. |
| Body that looks like PowerShell (`$Var = …`, `Get-Service`, `param(…)`) | **Skipped, returns empty findings.** | Detected by shebang or heuristic; bash shellcheck would just produce noise. |

**Practical takeaway for authoring:** declare every context input you read on the action form (Input Context tab), use the `LAST_*` exit vars for error reporting, and don't bother adding `# shellcheck disable=…` comments — the wrapper has already handled the false positives that are specific to Crucible's runtime, and anything that does fire is almost certainly a real bug worth fixing.

The validator runs in two layers:
- **Client-side (300 ms debounce):** `sh-syntax` WASM in the browser catches syntax-level errors instantly with zero server load.
- **Server-side (2000 ms debounce + on Save):** real `shellcheck` runs in the API pod for the full SC**** rule library. Instructor-accessible (the whole admin panel except the Audit Log is open to the `lab-instructors` group), 64 KB cap, 30 req/min rate limit, 3 s timeout.

Source: [`internal/scriptvalidator/wrap.go`](internal/scriptvalidator/wrap.go), [`internal/scriptvalidator/detect.go`](internal/scriptvalidator/detect.go), [`internal/scriptvalidator/validator.go`](internal/scriptvalidator/validator.go).

### 12.2 Platform `runner_smoke` timeout envelope

Do not infer the assessment runner's platform health from a shorter client
timeout. The dedicated hourly `runner_smoke` CronJob currently resolves an
8-minute pod-ready phase, a 10-minute assessment-run phase, and a 90-second
destroy phase. The monitor adds 30 seconds for ordinary request overhead and
reserves another 30 seconds for final cleanup, so the outer per-attempt context
is 20 minutes 30 seconds. Two attempts with a 30-second backoff authorize a
maximum 41-minute-30-second retry cycle. Its session JWT is minted for 46
minutes so a SIGTERM at the 45-minute Kubernetes Job deadline can still issue
the detached, 30-second-bounded cleanup DELETE. The pod has 60 seconds of
termination grace, and the schedule is hourly.

Those limits are one contract. The JWT must outlive the Job deadline plus
cleanup reserve while remaining shorter than the hourly schedule. A `401`
before cleanup finishes, a
`context deadline exceeded` before a configured phase can finish, or a Job
deadline/schedule mismatch indicates deployment/runtime drift rather than an
assessment authoring error. See
[`docs/instructor/troubleshooting.md`](docs/instructor/troubleshooting.md#runner_smoke-synthetic-check-is-firing).

---

## 13. When you should hand back to the human

- The user asks for a check that requires a credential or tool not in the Kali runner. Recommend they request the addition or use `vmware_tools` mode.
- The user asks for a workflow that would mutate the student's VM in a way that breaks subsequent assessments. Suggest a snapshot/revert design instead.
- The user asks you to bypass `status=approved` gating. That's an instructor-admin decision, not an authoring decision.

---

## 14. Template-health operator contract

Template-health paging uses persisted, independently confirmed state:

- `crucible_template_health_status{template}` is `1` healthy, `0` unhealthy, `-1` unknown. It is rebuilt from PostgreSQL and replaced in Pushgateway with `PUT`; it has no `check_type` label.
- `crucible_template_health_attempt_status{template,check_type}` and `crucible_template_health_last_check_timestamp_seconds{template,check_type}` are raw diagnostics only. Never page from a raw attempt.
- `crucible_template_health_checker_up` is included in every replacement snapshot. Leader startup performs a lightweight vCenter probe before replacing the group, so deploys do not leave the alert gate absent.
- A first deep failure persists pending state and schedules a durable `template_health_confirm` job after a 5-minute default backoff (hard-capped at 30 minutes). Only a failed fresh confirmation clone can mark deep health unhealthy; success clears pending state.
- The virtual-disk corrupt/unsupported vSphere observation is retained under fault class `vsphere_virtual_disk_corrupt_or_unsupported` and in PostgreSQL error text/logs. Do not suppress it.

This repository does not own live Grafana resources. Proposed confirmed alert:

```promql
(crucible_template_health_status == 0)
and on() (crucible_template_health_checker_up == 1)
and on()
  (time() - crucible_template_health_checker_last_success_timestamp_seconds < 30 * 60 * 60)
```

Use `for: 15m`; do not infer confirmation with a 13-hour `for`. Proposed annotations:

```yaml
summary: 'Confirmed template health failure: {{ $labels.template }}'
description: 'PostgreSQL marks {{ $labels.template }} unhealthy after the required independent confirmation. The checker is fresh; inspect per-check fault metrics, the admin health endpoint, and vCenter task history.'
runbook_url: 'https://github.com/jmal1/selfservice-api/blob/main/docs/instructor/templates.md#prometheus-and-grafana-contract'
```

Freshness alert:

```promql
absent(crucible_template_health_checker_last_success_timestamp_seconds)
or
(time() - crucible_template_health_checker_last_success_timestamp_seconds > 30 * 60 * 60)
```

Keep the paused live rule paused until migration + worker rollout completes, legacy `crucible_template_health_status{check_type=...}` series are absent, and the replacement metric agrees with `GET /api/v1/admin/templates/health`.

---

## 15. Image Upload → Auto-Import → Wizard Flow

This section documents the full lifecycle for getting an ISO or OVA into vCenter so it can be referenced by a template. This is an **operational** flow (not authoring), but it is documented here because AI agents sometimes need to explain it to instructors.

### 15.1 Lifecycle overview

```
Browser upload → MinIO staging → auto-import job → vCenter
                                                        ↓
                                        ISO: [NAS-BackupsAndISOS] ISOs/<file>
                                        OVA: vCenter Templates folder (VM moref)
```

**Statuses** (stored in `image_uploads.status`):

| Status | Meaning |
|--------|---------|
| `pending` | DB row created; browser not yet uploading |
| `uploading` | At least one presigned part has been issued |
| `uploaded` | Multipart complete; file confirmed in MinIO. Auto-import job enqueued. |
| `importing` | Worker is streaming file from MinIO to vCenter |
| `imported` | Terminal success. ISO: `datastore_path` set. OVA: `vcenter_vm_id` set. MinIO object deleted. |
| `error` | Terminal failure. `error_message` explains why. MinIO object **retained** for cheap retry. |

### 15.2 Auto-import

After `POST /api/v1/admin/images/{id}/complete` finalises the multipart upload and the byte count is confirmed, the API **automatically enqueues an `image_import` job**. Instructors do not need to take any further action after uploading.

- The import job is enqueued at upload completion, not on a delay.
- If the job enqueue fails (e.g. NATS is temporarily down), the upload is still safe in MinIO. The instructor can trigger import manually via `POST /api/v1/admin/images/{id}/import`.
- Completing the same upload twice (client retry) is safe: if the row is already past `uploaded`, the second complete returns 200 without re-enqueuing.

### 15.3 ISO picker in the template wizard

The template creation wizard's "ISO install" step calls `GET /api/v1/admin/vcenter/isos`, which returns a **merged** list of ISOs from two sources:

```json
{
  "isos": [
    {
      "name": "kali-2024.4.iso",
      "path": "[NAS-BackupsAndISOS] ISOs/kali-2024.4.iso",
      "source": "datastore",
      "disabled": false
    },
    {
      "name": "mint.iso",
      "path": "[NAS-BackupsAndISOS] ISOs/mint.iso",
      "source": "uploaded",
      "status": "imported",
      "disabled": false,
      "image_id": "..."
    },
    {
      "name": "ubuntu.iso",
      "source": "uploaded",
      "status": "importing",
      "disabled": true
    },
    {
      "name": "fedora.iso",
      "source": "uploaded",
      "status": "error",
      "error_message": "no space left on device",
      "disabled": true,
      "image_id": "..."
    }
  ]
}
```

- `source="datastore"`: found on the vCenter ISO datastore by the datastore browser.
- `source="uploaded"`: came through the image upload pipeline. `status` mirrors `image_uploads.status`.
- `disabled=true`: entry is not yet selectable (still importing, or errored). The wizard renders these greyed-out with a status label so instructors know what is in progress.
- De-duplication: if an uploaded ISO's `datastore_path` matches a file already returned by the datastore browser, only the datastore entry is kept (the datastore listing is canonical).

### 15.4 OVA behaviour

OVAs are imported into vCenter's Templates folder as a VM (moref stored in `vcenter_vm_id`). Disks land on `vcenter.datastore` (the VM datastore), not `isoDatastore`. They are **not** mounted as CD-ROM media and **never appear** in the ISO picker.

The wizard discovers them only through `GET /api/v1/admin/vcenter/ovas` (imported rows selectable with `source_ref`; uploading/importing/error rows disabled with a reason). Create a draft with `source_type=ovf` and `source_ref` set to that moref. The New Template wizard defaults `skip_generalize=true` for OVF/OVA (prepared appliance — GuestOps sysprep/cloud-init clean is skipped; verify/publish still run). Uncheck only if the OVA is a raw OS that still needs generalize.

Deleting an unreferenced OVA image destroys the exact imported VM (`vcenter_vm_id`). A 409 is returned while any template still references it. Retry a failed import with `POST /admin/images/{id}/import`. Pack a bare `.ovf` folder into a single `.ova` (tar of `.ovf` + disks) before uploading; the API rejects `.ovf`.

### 15.5 Error retry

If an import fails (`status=error`), the MinIO object is retained so retry is cheap. The instructor can retry via `POST /api/v1/admin/images/{id}/import` without re-uploading the file. The API resets the status from `error` → `uploaded` and enqueues a fresh import job.

### 15.6 ISO provision outcomes and staging-VM recovery

Provisioning from an ISO emits `create_vm` → `verify_disk` → `power_on` → `wait_install` → `detach_cdrom` → `boot_installed` → `wait_tools`, plus two conditional steps when the datastore is slow to allocate the new system disk: `repair_disk` (the disk was recreated once, before anything is installed) and `power_on_deferred`.

`power_on_deferred` is **manual-mode only**. The disk probed clean but the host could not open it at power-on, so the VM is handed over powered off and the template still advances to `configuring`; the `await_manual_install` message says the VM is powered off rather than claiming it is running. A manual build is never failed for this, because the operator supplies the power-on. Unattended modes cannot start without power, so they recreate the disk once and retry, and fail if it still will not open. Do not "fix" a slow datastore by making the manual path recreate the disk — that destroys the flat being allocated and restarts the wait, and it is what burned a completed Linux Mint console install onto an errored template.

Recovery from `error` has exactly one supported cleanup step, and the wizard actions are not interchangeable: **Cancel** destroys the staging VM, clears `vcenter_vm_id`, and returns to `draft`; **Retry** destroys nothing and returns 409 while a moref is recorded. A staging VM attached to an errored template cannot be promoted to a template — there is no such path — so work worth keeping must be copied out in vCenter before Cancel. See §17 for the full contract and [`docs/instructor/troubleshooting.md`](docs/instructor/troubleshooting.md) for the operator-facing version.

---

- Workflow/Action/Playlist Go models: [`internal/models/workflow_models.go`](internal/models/workflow_models.go)
- DB schema (the actual CHECK constraints): [`internal/database/migrations/000012_assessment_engine.up.sql`](internal/database/migrations/000012_assessment_engine.up.sql), [`000014_action_library.up.sql`](internal/database/migrations/000014_action_library.up.sql), [`000015_action_platforms.up.sql`](internal/database/migrations/000015_action_platforms.up.sql)
- Runner script library (the `run_action` / `ctx_set` source): [`deploy/runner/actions.sh`](deploy/runner/actions.sh)
- Runner executor (how scripts are launched): [`internal/runner/executor.go`](internal/runner/executor.go)
- Engine (how runs are orchestrated): [`internal/engine/engine.go`](internal/engine/engine.go)
- API routes: [`internal/api/routes/routes.go`](internal/api/routes/routes.go) — workflow + action endpoints under `/api/v1/admin/`
- Live action catalog (the *real* source of truth for available actions): `GET /api/v1/admin/actions` against your Crucible instance.

---

## 16. Student content-filter operator contract

Student pod traffic originates from `10.100.0.0/16` and uses fwpodv01
(OPNsense 26.1) as gateway and DNS. The approved policy:

- blocks adult/explicit, gambling, drugs, violence, social-media, and
  audio/video streaming categories;
- forces the OPNsense Unbound SafeSearch rewrites;
- blocks external DNS plus common DoH, DoT, DoQ, VPN, proxy, and Tor bypasses;
- has no TLS interception;
- accepts permanent deployment-reviewed admin allowlist entries only; and
- ships blocked events only, with 30-day retention configured in the external
  log store.

Global quick logged denies cover TCP/UDP 853, UDP 784 and 8853 (DoQ), UDP 443
(forcing QUIC/HTTP3 fallback), and external GRE, ESP, and AH. Intentional
traffic within `10.100.0.0/16` is preserved for the tunnel protocols. TCP 443
is not blocked globally; custom tunnels over ordinary HTTPS remain a residual
limitation.

The worker owns quick global/floating firewall rules scoped by source network.
In OPNsense 26.1 those rules are priority group 200000 and therefore evaluate
before per-interface broad pod passes (priority group 400000). They deliberately
have an empty `interface`, so dynamic VLAN-to-`optN` remapping cannot move the
policy behind or around a pod pass.

Content-filter activation is disabled by default. `categoryFeedBaseURL` must be
exactly `https://student-filter-feed.example.test` (an optional trailing slash is
accepted). The worker deterministically expands it to
`/lists/drogue.txt`, `/lists/agressif.txt`, `/lists/dangerous_material.txt`,
`/lists/audio-video.txt`, `/lists/social_networks.txt`, and
`/lists/weapons.txt`; arbitrary hosts and paths are rejected. These
OPNsense-reachable schema-2 lists are generated by the validated internal feed.
The capacity-tested built-in selection remains exactly `oisd2`, `hgz014`, and
`hgz021` (Gambling Mini); `hgz019` and `hgz020` are larger gambling variants,
and `hgz022` does not exist. Missing/invalid feed configuration fails before
any partial policy mutation.

Activation is also intentionally read-only in the worker today: it validates
configuration, inspects exact firewall/DNSBL state, and requires runtime
verification, but does not create, update, delete, refresh, or apply policy. This
prevents a failed multi-system activation from leaving a partial model that a
later unrelated firewall apply could activate. While policy is enabled but
inspection is unhealthy, the network reconciler also suppresses unrelated
firewall applies.

OPNsense 26.1's
built-in Force SafeSearch setting is a general/global Unbound switch, while the
approved scope must leave management and staging unchanged. Do not enable the
global switch. A reversible live pilot proved source-scoped SafeSearch can use
an `/usr/local/etc/unbound.opnsense.d/*.conf` fragment with
`access-control-view`, `view-first: yes`, and SafeSearch `local-zone` /
`local-data` rewrites. Crucible owns only
`/usr/local/etc/unbound.opnsense.d/crucible-student-safesearch.conf`; it never
edits OPNsense's generated global `safesearch.conf` or any other manual file.
The owner requires a pinned OpenSSH host public key, rejects unowned/conflicting
fragments, overlapping source CIDRs, and unsafe source CIDRs, atomically stages the exact fragment at
`/var/unbound/etc/crucible-student-safesearch.conf`, runs
`configctl unbound check` plus a direct exit-status-bearing
`unbound-checkconf`, and activates through `configctl unbound restart` followed
by a running-service check.
Before mutation it writes durable rollback state outside the `*.conf` include
glob. A later invocation recovers an interrupted transaction before accepting
the current file as a baseline. Every failure restores the persistent source
before any restart, rebuilds and verifies the staged copy, and retains the
durable backup when rollback is incomplete. Once activation and exact readback
are durably marked committed, backup-cleanup failure preserves the active
configuration for cleanup on the next invocation rather than rolling it back.
Rollback and ambiguous commit failures are surfaced rather than hidden.

This owner is not wired into `reconcileContentFilter`: activation remains
read-only. `SupportsSourceScopedSafeSearch` is true only for a client with
complete, valid SSH auth and host-key pinning; API-only clients report false.
Effective verification remains unsupported until the read-only synthetic can
use uncached controlled fixtures from a real student-source query path and an
unchanged control source. DNSBL apply is asynchronous, and its action can return
OK while masking shell errors; never trust API/model status alone.

### Firewall generated-rule ownership

Always inventory automation rules with `GET /api/firewall/filter/get`.
`searchRule` is not authoritative: it has returned `total=1` while thousands of
rules existed. OPNsense 26.1 writes the rule field `description`; sending the
legacy `descr` key silently creates an unnamed rule.

Automatic cleanup may delete only an exact pod-pass shape that is either:

1. a blank-description legacy rule, or
2. marked `crucible:pod-pass:v1:...`.

Every named/manual rule is preserved. A generated pod pass must be one `optN`
interface, pass/in/IPv4/any, one `10.100.x.0/24` source, destination `any`, and
empty ports/inversions. A partial or ambiguous match aborts mutation. Cleanup is
bounded per pass and applies once; a destroy that hits the bound remains
`destroy_failed` so retry can finish it.

The 2026-08-21 incident demonstrated why these constraints are mandatory:
fwpodv01 had 3,088 automation rules (3,078 blank descriptions), including 86
copies of `opt6 + 10.100.15.0/24 -> any pass`, and a 5,272,716-byte
`/conf/config.xml`. Root causes were unconditional creation in `CreatePod`,
missing destroy cleanup, VLAN/`optN` reuse, and the `descr`/`description`
mismatch. The supervised cleanup retained all ten named/manual rules plus one
pass for each of six assigned pod interfaces.

Do not deploy content-filter changes, scale workers, or restore destructive
synthetics while an infrastructure containment hold is active. Read-only policy
inspection through `content_filter_policy` is the only non-destructive
synthetic defined for this feature.

---

## 17. Provisioning maintenance operator contract

Crucible has two independent, strict maintenance controls:

- `PROVISIONING_ENABLED` controls API admission for new user provisioning.
- `WORKER_PROVISIONING_CLAIMS_ENABLED` controls whether workers claim
  clone-, create-, staging-, and import-capable jobs: `pod_create`, `vm_add`,
  `template_provision`, `template_generalize`, `template_verify`,
  `template_revalidate`, `template_health_confirm`, and `image_import`.

Both default to `true` for backward compatibility. If either variable is set,
it must parse as a Go boolean; an invalid value fails process startup rather
than silently enabling provisioning. Helm exposes these as
`provisioning.enabled` and `provisioning.workerClaimsEnabled`.

A never-started pending `pod_create` can now be cancelled directly by the API before any worker claim or external ownership evidence exists. In that case the delete path marks the create job terminal, marks the pending pod VM rows deleted, releases the VLAN, and never calls vCenter or OPNsense. Once a pod has a claim, a VM MoRef, a placement row, or a durable portgroup receipt, delete falls back to the normal destroy/manual-cleanup path.

During a claims-contained phase-1 rollout, the current Helm revision must
already render `provisioning.workerClaimsEnabled=false` before any migration or
image upgrade begins. A live `kubectl set env` override is not sufficient:
Helm rollback restores the prior rendered manifest and would erase that
override. First use `deploy/scripts/deploy.sh --prepare-claims-baseline` with
the exact currently deployed safe chart checkout. The script inventories every
rendered Deployment, DaemonSet, StatefulSet, CronJob, and Job container plus
the engine's dynamic `RUNNER_IMAGE`; it pins each one to the immutable sha256
ImageID reported by the corresponding healthy live pod. Missing, mutable,
mixed, or mismatched image evidence fails closed.

Stored Helm-manifest equality is not a no-restart proof. The baseline command
compares each safe-chart workload directly with its live resource. It permits
only the intentional claims setting, digest pinning, and removal of
`kubectl.kubernetes.io/restartedAt`; that annotation may restart a workload
only after every rendered image is proven digest-equivalent to the running
image. The candidate is server-defaulted with a dry-run create, then its
canonical spec is compared directly with the canonical live spec. It does not
use three-way `kubectl diff`, which can preserve live-only fields. Command,
environment, volume, security-context, replica, service-account, or other
workload drift blocks the baseline. The command never rolls a failed baseline
back to a claims-enabled revision.

Then run `--verify-rollback-containment`. Immutable revision **163** must remain
readable. Because Helm rollback creates a new latest revision, a newer revision
is accepted only when its manifest, hooks, complete effective values, and chart
metadata exactly match revision 163; its description is never trusted as proof.
The latest revision must be deployed, claims must remain false, there must be
exactly one worker, every live declared image and ImageID must equal the
persisted pin, all rollouts and retained CronJob/Job evidence must be healthy,
and PostgreSQL must be at migration 36 clean. Baseline
preparation proves the migration did not change. Stored, live, and newly
prepared baseline manifests must also keep content-filter activation disabled
with an empty feed. The normal deploy repeats the proof before pulling and
immediately before `helm upgrade --atomic`, rejecting an intervening Helm
revision so this immutable baseline is the immediately previous successful
rollback target.

The production candidate is also immutable and source-bound. `deploy.sh`
requires a clean, attached `main` checkout whose `HEAD` exactly equals trusted
`origin/main`, verifies that exact commit through GitHub, and requires one
successful `ci.yaml` push run containing successful builds for api-gateway,
provision-worker, crucible-engine, synthetic-api-monitor, and crucible-runner.
Every main push, including docs-only merges, builds all five even when path
filtering would rebuild only one on a pull request. CI publishes each image with
the full commit SHA and uploads a per-component digest record bound to that
workflow run. The deploy downloads those five records from the exact successful
run and requires GHCR's full-SHA tag to resolve to the identical digest. It also
rejects a digest carrying another full commit tag and reads the immutable OCI
config to require `org.opencontainers.image.revision` to equal the source SHA.
A floating tag, missing artifact/build, retagged older digest, or run/GHCR/OCI
identity mismatch fails closed.

Production candidates must also name an explicit UI source with
`--ui-source-sha`; omission never silently preserves the live UI. For this
rollout the source is `jmal1/selfservice-ui` merge
`6570e3034ad718c5840c38efc4c9f51781ce9f42`. The deploy proves that exact
commit's successful `ci.yaml` push run and successful `test` and `build` jobs,
then downloads the run's Buildx `.dockerbuild` record. Its exported digest,
repository, source revision, expected short-SHA tag, and builder run attempt
must agree, and the selected GHCR digest must carry the same immutable OCI
revision label. Exactly the UI container is replaced with that proven digest.

After immutable candidate rendering and server validation, but before the release lock, claims pause, or any live mutation, the preflight requires contiguous paired migrations with PostgreSQL at the exact clean head; zero pending, claimed, in-progress, or rollback create/import-capable provisioning jobs; one free pod slot for the active primary synthetic user; no nonterminal Job owned by the API-monitor, janitor, or runner synthetic CronJobs (including the pre-pod controller window); and digest-pinned candidate images whose OCI revision matches the exact API/UI source SHA. Operators capture baseline alerts before deployment and compare the post-deploy state, but a valid deploy is not blocked by a hard-coded allowlist. Every required query, read, parse, malformed, missing, or unknown result fails before lock acquisition. After claims are paused and all longer locked validation completes, the provisioning-job and live-state checks run again directly adjacent to Helm. A late pending job restores prior claims state and exits without invoking the upgrade.
Homelab Roadmap rows are outside this repository's authority, so the deployment
coordinator must prove that cross-repository gate before invoking `deploy.sh`;
there is no in-repo checkbox that pretends to prove it.

If the live worker has a temporary claims-enabled override while the candidate
render remains claims-disabled, candidate provenance, rendering, and the
first server dry-run still happen without mutation. A real apply then acquires
the release lock, verifies the currently deployed live release remains
content-identical and claims-disabled, deliberately sets the live worker back
to claims disabled, waits for that rollout and durable job drain, and then
runs the live/current-release rollback proof. The override does not create a
replacement checkpoint or dynamic rollback baseline.

Every other rendered image is copied from the exact live ImageID, including
PostgreSQL, NATS, NATS box/reloader, and future unrelated external chart
workloads.
Docker Hub aliases are canonicalized (`nats` equals `docker.io/library/nats`;
`natsio/x` equals `docker.io/natsio/x`) before repository comparison.
The final post-rendered manifest must contain only sha256 workload images and a
sha256 `RUNNER_IMAGE`, one worker, claims disabled, content-filter activation
disabled, and an empty content-filter feed. Kubernetes server-side dry-run
validates that final manifest before any lock or claims mutation, and `--dry-run`
prints that pinned manifest even when live claims are temporarily enabled.
For a real apply, the script then locks, repeats the immutable revision-163
content proof, pauses live claims, keeps the API monitor unsuspended with
lifecycle disabled, and suspends
the janitor/runner clone CronJobs before draining work. PostgreSQL must have
zero `claimed`, `in_progress`, or `rollback` durable jobs and zero nonterminal
synthetic `pod_create` or `pod_destroy` jobs, including `pending` rows left
after a CronJob has exited. Destroy rows may carry only `pod_id`, so the drain
joins `pods` and checks the authoritative pod name rather than trusting
`payload.pod_name`. Every Kubernetes Job in the namespace must also
have `.status.active=0`. Immediately before apply,
under the lock, it re-proves source identity, immutable revision-163
equivalence and containment,
migration and workload health, external live digests, and the final server
dry-run. Initial and final server-defaulted objects are canonicalized and
compared in full, so a changed command, environment, volume, or other non-image
field fails closed. Both job drains are checked again directly before Helm. Helm's
post-renderer byte-compares its apply-time output with the validated candidate before
`helm upgrade --atomic`; it cannot fall back to chart tags or restart unchanged
external workloads through mutable image references. Because Helm excludes
hooks from post-renderer input, the script separately renders the upgrade view
and extracts every `pre-upgrade` / `post-upgrade` hook. Each executable hook
image must already equal its commit-built or preserved external digest, and the
hook is server-dry-run validated; any hook that would need post-renderer pinning
is rejected before Helm can execute it.

The Playwright synthetic on the external UI runner host is an external Docker Compose
deployment. Its separately proven merge
`b61ca0c5a353d112b5ef8f97666ee528da115442` is not rendered, validated, or
deployed by this Helm workflow.

Production API synthetic feedback includes the mutating `pod_lifecycle` check, so the final candidate may intentionally render `SYNTHETIC_LIFECYCLE_ENABLED=true` once cleanup safety has been proven. Rollback containment still forces the API monitor back to active/non-lifecycle mode and suspends janitor/runner before it will accept rollback containment. The external `synthetic-ui.timer` on the UI runner host remains outside Helm and must be verified independently; Helm never mutates or proves that host-level timer.

An atomic Helm failure never falls through the EXIT trap. While retaining the
release lock, the script forces claims disabled, immediately reapplies the
current synthetic containment, and proves that the latest deployed rollback
revision has immutable revision 163's exact manifest, hooks, complete effective
values, chart metadata, image inventory, and `RUNNER_IMAGE`. Helm may record
rollback as a newer revision; revision number and description alone are not
treated as identity. The proof also requires
readiness, migration, active non-lifecycle API-monitor intent, suspended clone
CronJobs, zero nonterminal synthetic create/destroy work, and both durable and
Kubernetes job drains. A complete
proof releases the lock and returns the original Helm failure. Any failed proof
retains the lock for manual intervention. After Helm reports success, the
deployed full object set and every live ImageID must equal the exact candidate.
For the CronJob, the deploy creates and awaits a fresh contained Job rather than
trusting a retained Job from the prior template. All workloads must be healthy,
and both drains must remain empty before release.

Baseline and application mutations hold the cluster-visible
`configmap/selfservice-phase1-deploy-lock`; every release mutation must use this
script and honor that lock. Remove a stale lock only after proving its recorded
holder is no longer active. Do not build, push, migrate, or upgrade phase-1
images until those proofs pass.

When API admission is disabled, the first instruction in each of these
handlers rejects the request before parsing, allocation, or database access:

- `POST /api/v1/pods`
- `POST /api/v1/blueprints/{blueprintID}/deploy`
- `POST /api/v1/pods/{podID}/vms`

The response is `503 Service Unavailable`, includes `Retry-After: 300`, and
uses the normal error envelope:

```json
{
  "error": "Provisioning is temporarily unavailable for maintenance.",
  "request_id": "..."
}
```

Delete/destroy, delete-VM, power, and cleanup paths are intentionally not
gated. When worker provisioning claims are disabled, ordinary clone-, create-,
template-staging/validation, and image-import jobs are excluded inside the
atomic claim query and remain pending; destroy jobs and any withheld type marked
`cleanup_only` remain claimable.

`template_provision` owns template terminal state only through its lease-fenced job lifecycle: retryable clone or ISO failures keep `templates.template_state=provisioning` while one atomic job update records `error`/optional `raw_error`/`attempts` plus preserved `first_*` and refreshed `current_*` failure fields, increments `retry_count`, clears claim timestamps, and schedules `next_attempt_at`; a later claim preserves that result through `in_progress`, success replaces it with the success envelope, and only a non-retryable or exhausted owned attempt may atomically commit both `jobs.status=failed` and the matching payload-selected template's `provisioning`→`error` transition. Lease loss, retry-write failure, malformed ownership identity, or either failed terminal write must leave the template unchanged for stale-job recovery, and transition metrics are emitted only after the terminal transaction commits.

ISO provisioning treats the pre-power-on disk probe as the only fail-closed disk gate. A probe failure recreates the system disk at most once and re-probes; a disk still unreadable after that recreate fails the job before an operator invests a manual install. Once the probe is clean, a power-on that faults `IsDiskNotReadyErr` is handled per mode: `unattend_mode` other than `manual` recreates once and retries the power-on because an unattended install cannot begin without power, while `manual` **must not** recreate and **must not** fail — it emits a `power_on_deferred` progress step, hands the VM over powered off, and still advances to `configuring`, because the operator supplies the power-on anyway and erroring strands a usable staging VM on a row whose only outbound edge is `error`→`draft`. `repairSystemDisk` reads power state before destroying anything and refuses when the VM is powered on or its power state cannot be read, since `RecreateSystemDisk` deletes the disk's backing files and would discard an OS installed over the console.

Staging-VM disposal is explicit, not implicit. `POST /admin/templates/{id}/cancel` accepts `configuring`, `ready`, and `error`, and is the only path that destroys the staging VM, clears `vcenter_vm_id`, and returns the row to `draft`, in that order, so a vCenter failure leaves both the state and the moref intact for another attempt. `POST /admin/templates/{id}/retry` destroys nothing and therefore returns 409 while `vcenter_vm_id` is non-empty, naming the moref and the cancel URL: `buildTemplateVMName` is deterministic, so re-provisioning would be blocked by preflight PF-09 or fault `DuplicateName` in `CreateBlankVM`, and the abandoned VM would be reaped. This applies to errored clone templates as well as ISO ones. The template-orphan reconciler's DB pass reads each candidate's power state before `destroyTemplateOrphanVM` (which power-cycles off and ignores the result), skipping and counting powered-on VMs as `skipped_powered_on` and unreadable ones as `skipped_undetermined`; a VM that no longer exists is not treated as undetermined, so its dangling moref is still cleared. Both counters are exported on `crucible_template_orphans_total`.

`VCENTER_HOSTS` is the single canonical allowlist for both VM placement and
standard-vSwitch portgroup mutation. Worker startup resolves every configured
entry against the configured datacenter and fails if the list is empty, has
duplicates, is missing from inventory, is ambiguous, or resolves without a
complete immutable host/compute-resource identity. The resolved host names,
inventory paths, and MoRefs are logged and frozen for the process lifetime.
There is no second placement-host setting.

Every clone, blank-VM create, and OVA import carries an explicit allowed
`HostSystem` plus a compatible configured resource pool. The shared placement
resolver requires the destination host to be connected, outside maintenance
mode, in the selected pool/source compute resource, able to access the target
datastore, and equipped with the required standard portgroup and capacity. It
does not fall back to `DefaultResourcePool` or unpinned DRS. Template staging,
publish/credential smoke clones, deep template-health clones, pod creation, and
add-VM all use the same resolver. An existing, resumed, or recovered VM on a
host outside the current allowlist is never reused, powered on, reconfigured, or
destroyed automatically.

Every production clone path inspects `config.hardware.device` on the source
before submitting `CloneVM_Task`. If the source contains a `VirtualTPM`, the
per-operation `VirtualMachineCloneSpec.TpmProvisionPolicy` is `replace`, so the
destination receives a new vTPM identity and cannot access TPM-sealed secrets
from the source. A hardware-property read failure blocks the clone; sources
without a vTPM leave the policy unset and retain vCenter's existing behavior.
This control does not recrypt the VM or its disks, so a destination may retain
the source's configuration encryption key ID. Config-key uniqueness is not
currently a Crucible invariant. Windows sources with vTPM must therefore have
BitLocker fully decrypted and protection off before they are used as clone
sources. If a vTPM is added to a staging VM later, decrypt it before that VM is
sealed or generalized.

Multi-cluster pod cloning uses `template_source_replicas`: one validated source
VM per logical template and immutable compute-resource identity. Operators
register a real source with
`POST /api/v1/admin/templates/{templateID}/source-replicas` and
`{"source_ref":"vm-123"}`; list and delete use the same collection path and
`/{replicaID}`. Registration resolves the VM, current host, compute type/MoRef,
and inventory path live through vCenter before storing a `ready` row. The
source's current host does not need to be in `VCENTER_HOSTS`: registration only
records inventory identity and does not make that host eligible for placement
or mutation. Target eligibility remains controlled exclusively by the frozen
`VCENTER_HOSTS` set and compatible configured resource pools. The migration
never fabricates replicas. A template retains its legacy
`vcenter_template` behavior only until its first replica is registered. That
registration durably enables source-replica mode; deleting every replica does
not restore legacy fallback and leaves provisioning blocked until a `ready`
replica is registered. `GET` returns both `replica_mode` and `replicas` so an
empty enabled configuration is visible. The resolver never
crosses compute resources: source replica, target resource pool, and exact
target host must share the same immutable compute identity.

Retained source replicas for a new compute resource are built through
`POST /api/v1/admin/templates/{templateID}/source-replica-builds`, not a
one-shot govmomi helper. The instructor-gated request identifies a ready source
replica, an idempotency key, destination name, and exact compute/host/pool/
datastore/folder MoRefs plus matching paths/names. Build-target validation is
intentionally independent of `VCENTER_HOSTS`, so a host outside the allowlist
can receive this one retained build without becoming eligible for pod,
template-staging, health, or any other normal placement. The service account is
preflighted for the entity-scoped clone/create/delete/snapshot/advanced-config/
pool/datastore privileges, including clone permission inherited by retained
destinations, and `Cryptographer.Clone` only for vTPM sources. TLS
uses the existing strict vCenter client; no build setting may weaken it.

`template_source_replica_builds` and its job form a durable, claim-fenced state
machine. Immutable inputs and a prepared/submitting phase are committed before
each vCenter call. Clone, snapshot, linked-clone canary, canary cleanup, and
residue-cleanup task MoRefs are reconciled after timeout, restart, or failover.
Clone, snapshot, and linked-clone creation are never blindly resubmitted.
Destroy submission is the narrow exception: a successor may safely resubmit it
only after revalidating the exact MoRef plus build, operation, and kind markers.
Name collisions, marker mismatch, or ambiguous lineage fail closed. Operator
recovery is limited to status, phase-aware retry, exact cleanup, and
operation-scoped retirement; there is no broad delete. `resume_phase` accepts
only forward phases and cleanup failures never replace it. A second recovery
request is rejected while the linked job remains pending or owned.

The retained VM is a powered-off full clone of the ready source anchor's
`base-image` snapshot with
`moveAllDiskBackingsAndDisallowSharing`, the exact requested destination, and
the shared vTPM clone policy (`replace` when the source has a vTPM). Validation
requires exact source snapshot and destination inventory identity, powered-off
state, no pre-seal destination snapshot, firmware/Secure Boot/vTPM-count/
security-provider and encryption-state parity, independent persistent disks with
no parent or source backing reuse, no connected or retained ISO, and distinct
public EK certificate/CSR hash sets for vTPM sources. An unencrypted source must
produce an unencrypted destination; an encrypted source requires a non-empty
destination configuration key from the same provider. Config-key uniqueness is
deliberately not required and the operation does not rekey.

After creating the retained `base-image` snapshot, acceptance creates but never
boots a linked clone on the configured provisioning datastore, verifies each
disk has an exact parent in the retained backing chain, and exactly destroys and
reconciles the marked canary. The destination replica remains `pending` or
`unhealthy` until cleanup is proven absent. One final transaction rechecks the
compatible ready source anchor and then promotes the destination replica and
build together. This proves cloneability only; it does not claim guest/L1
health. If a build never becomes ready, exact retained-VM cleanup atomically
persists `residue_cleaned_at` and deletes only that build's non-ready replica
reservation. A new idempotent build can then reuse the compute; the cleaned
operation itself cannot resume forward work. Template deletion preflights all
active, accepted, and not-fully-cleaned build ownership before any staging-VM
destroy.

The same exact-cleanup endpoint retires a successful `ready` build. Retirement
first disables the result replica so new placement cannot select it, rejects
every existing `vm_placements` reference or live downstream build that uses the
result as its source anchor, and requires the original distinct source anchor
to remain ready. It destroys only the exact result VM whose
build/operation/kind markers match, reconciling a lost destroy response before
safe exact resubmission. One transaction removes the result replica and records
the build as `retired` only after vCenter absence is proven. The source anchor
is never a retirement target, and retired history no longer restricts direct
replica or template deletion. Template deletion and replica-build admission or
restart share a per-template PostgreSQL advisory lock, and deletion rechecks
build safety while holding it before any vCenter destroy. A failed downstream
build releases its source reference only after it either never submitted a VM
or persisted exact residue cleanup. A forward retry locks and revalidates its
exact source as ready, so it cannot race an upstream retirement. Metrics are
`crucible_template_replica_build_total`,
`crucible_template_replica_build_duration_seconds_{sum,count}`,
`crucible_template_replica_build_phase`,
`crucible_template_replica_build_stuck`, and the last-success/failure
timestamps. The alert/runbook contract is in
`docs/instructor/templates.md#durable-retained-replica-builds`.

For a pod create, the worker resolves every VM's complete source/compute/pool/
host plan and commits the complete set to `vm_placements` before the first
standard-switch mutation. A partial durable plan fails closed. Retries reuse
the persisted identities rather than placing again. Clone-operation markers
carry the same logical template, replica, compute, pool, and host identities.
An old unsubmitted (`prepared`) clone operation can be replaced safely; an old
armed/submitted operation without the full identity requires manual cleanup.
For a job adopted before `vm_placements` existed, the worker reconstructs the
legacy source, exact live host, configured pool, and compute identity from
vCenter without charging the already-resident VM against headroom a second
time, then installs the per-VM DRS override. This compatibility path is
available only before source-replica mode is enabled; replica identity is never
fabricated for an existing VM.

`VCENTER_PLACEMENT_RESERVED_MEMORY_MB` is an optional comma-separated
`host=megabytes` map. Every key must be in `VCENTER_HOSTS`, duplicates and
invalid/non-negative values fail startup, and placement rejects a host when the
requested VM, earlier VMs in the same pod, and durable reservations from other
unreleased placement plans would consume its configured reserve. Each
unmaterialized VM stores its exact RAM reservation in `vm_placements`.
Admission takes transaction-scoped PostgreSQL advisory locks for all selected
hosts in stable order, then atomically checks every reservation that has not
been explicitly released before committing the plan. Retries reuse the same
rows; already-resident VMs record a zero reservation because live vCenter free
memory already includes them. A reservation is released only after the VM is
running or exact compensation proves no resource remains. Job terminal status
alone never releases it, so ambiguous/manual-cleanup outcomes remain reserved.
Database-clock release timestamps also keep a reservation visible to any
capacity sample taken before its release. This is a minimum safety floor for
critical resident workloads, not a quota.

Standard portgroup creation is scoped to the union of hosts selected by the
durable pod plan, not every allowlisted host. It records one durable per-host
receipt in `pod_portgroup_receipts` before the first vCenter mutation. The
ledger is independent of mutable rollback checkpoints. Its monotonic
`planned` -> `applying` -> `active` states prove whether switch mutation was
ever authorized; the rollback step is durable before `applying`. Each entry contains
immutable host/compute identity, expected VLAN, `vSwitch0`, inherited security
policy, whether the portgroup already existed, and the stable per-host vSphere
`HostPortGroup.Key` captured before the receipt becomes `active`. An
`applying` receipt that is both keyless and absent is ambiguous and fails
closed. Receipts backfilled from pre-ledger rollback data are marked `legacy`
and can never bind a current inventory key or authorize deletion. A changed key
or configuration also fails closed. A successful rollback records a durable
`removed` tombstone, so a later user destroy can recognize completed cleanup
without treating broad name absence as ownership proof. A partial failure
removes only portgroups newly created by that receipt, in reverse order. Rollback and destroy
never infer ownership from OPNsense VLAN state and never delete preexisting
portgroups. A missing, malformed, legacy receipt without immutable host
identities, or historical receipt naming a host outside the current allowlist
fails closed for manual escalation. The destroy job reports
`manual_cleanup_required`, the pod remains `destroy_failed`, and its
VLAN/interface allocation remains reserved; operators must inspect or backfill
exact ownership rather than broaden the allowlist. All
AddPortGroup/RemovePortGroup sections, including compensation, hold the stable
PostgreSQL session advisory lock on one acquired connection; no database
transaction is held across vCenter calls. A transient deletion failure also
leaves the pod `destroy_failed` and retains its VLAN/interface allocation for a
safe retry. Add-VM currently selects only from hosts already covered by that
pod's receipt; it never expands switch ownership implicitly.

Cluster placements receive a per-VM DRS override with `Enabled=false`; a
standalone `ComputeResource` records the equivalent `standalone` control.
Before any network or clone mutation, the worker performs a read-only privilege
check for `Host.Inventory.EditCluster` on every selected
`ClusterComputeResource`. The vCenter service account must hold that privilege
on each selected cluster; standalone computes do not require it. Configuration
still fails closed if the control cannot be installed. Resume, power,
snapshot, revert, and suspend paths compare the VM's live host, compute
resource, and DRS override with `vm_placements` before forward mutation.
Automatic or manual movement is reported as placement drift rather than silently
changing the durable receipt. Only a successfully read host, compute, or DRS
mismatch is typed as proven drift and escalated to manual cleanup. vCenter
connection, timeout, and property-read failures retain their operational cause
so normal retry policy remains available. The pipeline exporter publishes
`crucible_vm_placement_total`,
`crucible_vm_placement_headroom_megabytes`,
`crucible_vm_placement_drift_total`, and
`crucible_vm_placement_rejections_total`.

Periodic template-health and credential revalidation scheduling remain keyed to
the logical template in this foundation. Replica registration and each placement
resolve the source live, so a missing or moved source fails closed, but
independently confirmed per-replica health/credential cadence is a subsequent
layer. Do not infer replica health from the logical-template health metric.

Compensation intent is durable before clone submission. The fenced job first
persists a per-attempt operation UUID, pod id, pod VM id, target name, source
template identity, selected host name/MoRef, and resource-pool MoRef. Immediately
before `CloneVM_Task`, it atomically arms `cleanup_only` and embeds the operation
UUID, source identity, pod VM id, host MoRef, and pool MoRef in the clone's
vCenter `extraConfig`. If vCenter accepts the request but the SOAP response is
lost, recovery searches the target folder by that complete marker; a same-name
VM without the marker is ambiguous and is never adopted or deleted.
Once vCenter returns a task MoRef, the worker persists it before waiting.
Successors resume that exact task and never submit a second clone for an armed
operation. Template verification and revalidation smoke clones use this same
protocol; their job and template ids provide the immutable operation scope.
For `clone_with_customize`, the generated guest credential is also durable
before clone submission and is reused unchanged across retries; it is never
replaced with the template's `REPLACE_WITH_BUILD_PASSWORD` build password. The clone operation
UUID is the cloud-init / Cloudbase-Init instance identity, so every clone is a
new first-boot instance while a resumed operation remains stable.
Migration 000036 adds explicit guest-credential acceptance timestamps to both
templates and pod VMs. A customized template is not provisionable until a
fresh smoke clone authenticates and records template acceptance; legacy active
templates start unaccepted and must be revalidated. Static template kinds are
the only exemption.
Transient task waits before clone acceptance consume the normal forward retry
budget by resuming that exact persisted task. A forward retry clears only the
cleanup dispatch marker and retains the operation, task, source, compute, pool,
and host identities. Once the exact clone VM is accepted and staged, any
placement validation or configuration failure enters cleanup-only compensation
immediately; retries reconcile and destroy that exact VM and cannot replay
VLAN, interface, DHCP, firewall, portgroup, or clone setup.

After power-on, a customized pod VM remains `configuring` until VMware Tools
accepts the exact generated `student` (Linux) or `Student` (Windows)
credential and the worker durably records acceptance for that exact pair and
vCenter VM MoRef. Clone adoption, replacement, and cleanup clear the marker, so
a retry cannot transfer acceptance from a destroyed clone to its replacement.
The worker polls for up to six minutes to allow cloud-init / Cloudbase-Init and
their reboot to complete. Failure enters compensation; it must never be
represented as a credential-ready VM or active pod. Status `running` alone is
not sufficient: legacy customized VMs without a marker matching their current
MoRef keep their credentials hidden.
`clone_no_customize` and `registered_existing_vm` never receive guestinfo
credential injection and bypass this generated-credential authentication gate;
their non-empty persisted static template credentials remain authoritative.

Clone task waits have a 15-minute operational deadline and honor lease loss.
Task or marker recovery stages the exact VM MoRef before any mutable
reconfiguration. Cleanup never resolves a VM by display name and never performs
forward configuration, power-on, or snapshots. Before each destructive retry,
it revalidates the VM's live resource pool plus the persisted operation, source,
replica, logical-template, pod-VM, compute, pool, and host `extraConfig`
markers. Any confirmed mismatch fails closed as identity drift; an operational
property-read failure remains retryable. A pre-configuration clone can
legitimately lack its DRS override; exact cleanup installs and verifies the
missing override before deletion, while an existing enabled override remains
proven drift and fails closed. An armed submission with neither a
task nor a marked VM remains cleanup-only while reconciliation is credible;
after 30 minutes it becomes `manual_cleanup_required` rather than retrying or
submitting a duplicate indefinitely. For pod creation, rollback identity is
persisted before clone adoption and the cleanup marker plus operation identity
are disarmed only by the same fenced claim afterward. VM rows move to terminal
`error`/`deleted` states only after exact cleanup succeeds, preventing template
deletion from erasing unresolved cleanup identity.
If ownership is lost or cleanup fails, the parent job remains `cleanup_only`
and retries with capped backoff independently of its original provisioning
retry budget. It cannot resume cloning, power-on, or snapshots. A cleanup-only
`pod_create` may move a `provisioning` pod to `error` only when an exact staged
target belongs to a VM in the job; marker-only, mismatched, active, pending, and
unknown states fail closed.

If the database write that returns cleanup work from `in_progress` to `pending`
fails, the live worker does not abandon or terminalize it. It retries that
idempotent write in-process with a 10-second per-write deadline and exponential
backoff from 1 to 30 seconds until persistence succeeds or the worker shuts
down.

Every worker process uses a unique `hostname-UUID` identity and every claim adds
a second UUID fencing token. Active jobs renew `claimed_at` every 30 seconds and
use a conservative 15-minute lease.
Startup and the one-minute recovery pass reset only claims expired according to
the PostgreSQL clock; they never broadly reset another replica's fresh work.
Loss of ownership or lease freshness cancels execution. In-progress, retry,
terminal-status, cleanup-target staging, and clone-adoption writes all verify
the same claim owner, so a superseded worker cannot finalize or attach a clone.
If ownership is lost while recording a completed infrastructure step, the old
worker does not run destructive undo. Any ambiguous rollback-receipt persistence
uses the same non-destructive handoff path. Before every rollback undo, the
worker re-persists the receipt under its current claim, refreshing the lease and
failing closed if ownership cannot be proven. The handoff idempotently appends
the exact receipt, fences the current generation into `cleanup_only`, and lets
the successor perform rollback. Worker shutdown, including scheduler and
database-pool closure, waits at most two minutes; the chart grants 150 seconds
of termination grace. Any unfinished claim then becomes recoverable only after
its lease expires.

Completed compensation finalizes the parent job as `failed` with
`compensated: true` and publishes a `compensated` event; it is never reported
as successful provisioning. Missing or ambiguous immutable ownership proof
finalizes with `manual_cleanup_required: true` instead of risking deletion of
an unrelated VM. No production worker scale-up is implied by this contract.

### Host-isolated canary prerequisite

During ESXi host containment, production must keep API admission disabled,
worker provisioning claims disabled, worker replicas at zero, destructive
synthetics disabled, and all clone-capable schedulers disabled until an operator
opens the canary. The canary configuration must set `VCENTER_HOSTS` to a single
allowlisted host and `VCENTER_RESOURCE_POOLS` to the matching pool only. The pool
restriction is defense in depth, not the host-isolation boundary.

The worker installs a per-VM DRS-disabled override after clone creation and
checks it before later forward operations. That control does not cover the
interval between clone submission and override installation, cannot prevent a
manual vMotion, and is not a substitute for containment policy. Before any
canary worker is started, a vCenter administrator must create and verify an
external DRS VM-host affinity/must-run rule (or equivalent host exclusion) that
keeps all Crucible-created and temporary VMs off the contained host, and must
verify that no automated vMotion policy can move them there. Do not claim
code-only isolation. Do not widen `VCENTER_HOSTS` to work around a placement
diagnostic. Keep the pending production pod/job untouched until the canary is
explicitly approved. Live topology and allowlists live in private
crucible-deploy / the ops vault — not in this public chart's example values.

Authenticated clients and the non-destructive API synthetic use
`GET /api/v1/provisioning/status`. Its complete stable response contract is:

```json
{"enabled": false, "message": "Provisioning is temporarily unavailable for maintenance."}
```

or:

```json
{"enabled": true, "message": "Provisioning is available."}
```

`SYNTHETIC_PROVISIONING_EXPECTED_ENABLED` declares which state the monitor
expects. When it is `false`, the main synthetic registry runs only read-only
checks; POST-based RBAC probes and pod lifecycle creation are omitted. The
separate janitor remains permitted because it only exercises the preserved
delete/cleanup path. Admission observability is published through
`crucible_provisioning_admission_enabled` and the bounded-route counter
`crucible_provisioning_admission_rejected_total{route=...}`.
