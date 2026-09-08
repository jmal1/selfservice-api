# Runner Environment

When a workflow or action runs, the engine spins up (or reuses) a
**runner** — a small Kali pod on the student's VLAN — and executes
your script there. This page documents what's available inside that
runner so you can write scripts that "just work" without hunting
through engine source.

Everything described here applies to `execution_mode: kali_runner`.
The `vmware_tools` mode is different; see the bottom of this page.

---

## Network position

The runner pod lives on the **student's VLAN** with full IP
connectivity to every VM in their pod. You can SSH, ping, nmap, or
otherwise interact with targets exactly as a student on the same network
would.

```
[ Runner (Kali) ]  →  [ Target VM(s) on same VLAN ]
        │
        └── No outbound internet (firewall denies)
```

> [!warning]
> The runner has **no outbound internet access**. If your script tries
> to `curl https://github.com/...` or `apt install` something, it will
> hang or fail. Everything the runner needs must be pre-baked into the
> runner image. File a request with an admin if a tool is missing.

---

## Standard environment variables

The engine injects these into every workflow and action invocation.
Don't guess paths or IPs — use the env vars.

| Variable | What it is | Example |
|---|---|---|
| `CRUCIBLE_TARGET_IP` | Primary target VM's IP on the pod VLAN | `10.50.12.20` |
| `CRUCIBLE_TARGET_USERNAME` | Default SSH user on the target | `student` |
| `CRUCIBLE_TARGET_PASSWORD` | Default password for that user | |
| `CRUCIBLE_TARGET_OS` | OS family hint | `linux` / `windows` |
| `CRUCIBLE_POD_SUBNET` | The pod's VLAN CIDR | `10.50.12.0/24` |
| `CRUCIBLE_POD_INDEX` | The pod's VLAN tag, a stable number unique to the pod | `119` |
| `CRUCIBLE_WORKDIR` | Per-workflow scratch directory, deleted after the run | |
| `CRUCIBLE_SOCKET` | Socket `run_action` reports to | |
| `CRUCIBLE_CONTEXT` | Per-workflow JSON context file | |

That is the complete list, plus the `CTX_*` values earlier actions published.
`PATH`, `HOME`, `TERM` and `LANG` are inherited.

> **Not provided:** `CRUCIBLE_TARGET_HOSTNAME`, `CRUCIBLE_POD_ID`,
> `CRUCIBLE_RUN_ID`, `CRUCIBLE_WORKFLOW_SLUG`, `CRUCIBLE_PLAYLIST_SLUG`,
> `CRUCIBLE_STUDENT_USERNAME` and per-slot variables like
> `CRUCIBLE_TARGET_WEB_IP` were previously listed here but have never been set by
> the runner. A script referencing one gets an **empty string**, and the script
> validator declares these names so it will not warn you — so the mistake shows up
> only as a confusing runtime failure (`ssh student@` with no host).

> **Also not provided: `PARAM_*`.** An action's `params` field is stored and
> never read; nothing turns it into an environment variable. Library actions
> take `--flag value` arguments instead. See
> [Building library actions](actions.md).

Address the target with `CRUCIBLE_TARGET_IP`.

### Which VM does a workflow run against?

A workflow runs against **one** VM: the pod's *primary* VM, chosen by `boot_order`
ascending, ties broken by creation time and then VM id. All of a pod's VMs are
created together, so they normally share a creation time and `boot_order` of 0 —
which means for a multi-VM pod you should **set `boot_order` in the blueprint** to
declare which VM is the one being assessed. The tiebreaker guarantees the same VM
is picked every run either way, but only `boot_order` makes that choice meaningful.

The VM that was assessed is recorded on the run and displayed as **Assessed VM** on
the run detail page, so a result can always be traced to the machine it came from.

---

## Authentication to the target

SSH keys for the default user are pre-installed on the runner. You can
SSH without a password:

```bash
ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 \
    "$CRUCIBLE_TARGET_USERNAME@$CRUCIBLE_TARGET_IP" \
    'whoami'
```

Always pass `-o ConnectTimeout=5` (or similar). Without it, a hung target can
stall your script until the engine kills it at the workflow `timeout_seconds`
(default 300s).

For Windows targets, WinRM is configured with the equivalent of an
auto-login NTLM cred:

```bash
crucible-winrm "$CRUCIBLE_TARGET_IP" "Get-Service WinDefend"
```

(The `crucible-winrm` wrapper is in `/opt/crucible/bin`.)

---

## Pre-installed tools

The runner is Kali Linux with a **curated** toolkit — not the full Kali
metapackage. The image is pulled fresh for every assessment run, so it is
deliberately kept under 3 GB.

The authoritative list is
[`internal/runnertools/tools.txt`](../../internal/runnertools/tools.txt).
The Dockerfile derives both its install list and its verification step from
that file, and a guard test fails the build if this table drifts from it — so
what you see here is what is actually in the image.

| Category | Tools |
|---|---|
| Shell & runtime | `bash`, `python3`, `pip3`, `git`, plus coreutils (`awk`, `sed`, `grep`, `cut`, `sort`) |
| Network | `nmap`, `nc`, `socat`, `ping`, `ip`, `dig`, `ssh` |
| Web | `curl`, `wget`, `nikto`, `whatweb` |
| SMB / Active Directory | `smbclient`, `smbmap`, `netexec`, `enum4linux-ng`, `ldapsearch`, `impacket-secretsdump` |
| Credentials | `hydra`, wordlists at `/usr/share/seclists` |
| Data stores | `redis-cli` |
| Crypto & parsing | `openssl`, `jq` |
| Crucible helpers | `/opt/crucible/lib/actions.sh` (source this), `/opt/crucible/bin/crucible-runner` |

> [!warning]
> Tools **not** in the image include `gobuster`, `wfuzz`, `hashcat`,
> `tcpdump`, `mtr`, `iperf3`, `httpie`, `yq`, `xmlstarlet` and `sqlmap`.
> Earlier versions of this page listed some of them by mistake. Calling one
> exits **127** and the action is reported as an `error` naming the missing
> command.

If you need a tool that is not in the runner image, **do not `apt install` it
from your script**: the runner has no internet egress, and you would be mutating
a shared image. Ask an admin to add it to `tools.txt` and rebuild.

The workflow editor warns at save time (**CRU0002**) when a script calls a
command the runner image does not provide, so you normally find out while
authoring rather than at grading time. CRU0002 looks only at **command
position**, so ordinary shell plumbing is not flagged: variable assignments
(`TARGET=example.com`), bash array appends (`curl_args+=(-b "$jar")`), comments,
and single-quoted text are all ignored. If it names something that clearly is
not a command, report the linter bug rather than working around it.

Crucible's own shipped library actions are held to a stronger version of the
same rule: their tool dependencies are checked **at build time**, and a library
action that calls a command missing from `tools.txt` fails CI rather than
shipping. The difference is deliberate — a warning is the right level for your
own drafts, but a missing tool in a shipped library action would exit 127
in the middle of someone else's graded assessment.

### Calling `run_action`

The runner helper takes a display label followed by the command or function it
must execute:

```bash
run_action "<display label>" <command-or-library-function> [args...]
```

Inline commands keep their normal executable name:

```bash
run_action "HTTP probe" bash -c 'curl -fsS --max-time 5 "$CRUCIBLE_TARGET_IP"'
```

Database library slugs are kebab-case, but the generated bash functions sourced
from `/opt/crucible/lib/library.sh` are snake-case. For slug
`demo-http-service-reachable`, call:

```bash
run_action "DEMO - HTTP Service Reachable" demo_http_service_reachable
```

A label without a second argument has nothing to execute. Passing
`demo-http-service-reachable` as that second argument asks the shell for a
nonexistent command instead of the injected function. Both forms are rejected
when the workflow is activated.

---

## Filesystem layout

| Path | Contents |
|---|---|
| `/opt/crucible/lib/actions.sh` | Shell helpers (`run_action`, context emit); sources the generated library |
| `/opt/crucible/lib/library.sh` | Per-run database library actions as snake-case shell functions |
| `/opt/crucible/bin/` | Crucible CLI wrappers (winrm, context, etc.) |
| `/tmp/` | Scratch space; cleared between runs |
| `/var/tmp/run-<RUN_ID>/` | Per-run scratch; preserved until the run completes |

Anything in `/var/tmp/run-<RUN_ID>/` is uploaded as a run artifact at
the end of the workflow — useful for capturing student-pod log dumps
for forensic exercises.

---

## Time budget

| Setting | Default | Max |
|---|---|---|
| `setup_script` timeout | 60s | 120s |
| `script` timeout (`timeout_seconds`) | 300s | 1800s |
| Single `run_action` timeout (engine-imposed) | 60s | 300s |

Use the **smallest reasonable timeout** for each `run_action`. A long
per-action timeout makes the run feel slow and masks real hangs. If a check
needs more than 30 seconds, consider whether it does too much in one step.

---

## What about `vmware_tools` mode?

In `vmware_tools` mode the script runs **inside the target VM** via
the VMware Tools guest-ops API, not on the runner. Differences:

- Environment vars are **not** injected. You're in the target guest OS,
  with whatever PATH/HOME that user has.
- No `/opt/crucible/lib/actions.sh`. You can't `run_action`. The whole
  script becomes one anonymous step.
- `guest_interpreter` controls what runs your script: `/bin/bash`,
  `cmd.exe`, `powershell.exe`.
- Network: whatever the target VM has access to — generally none, since
  pods are firewalled.

Use `vmware_tools` mode when you specifically need to inspect local
state on the target (file presence, registry keys, local services) and
SSH-from-runner isn't practical.

---

## See also

- [Building Workflows](workflows.md) — putting env vars to use
- [Troubleshooting](troubleshooting.md) — when env vars are wrong
- [Overview](overview.md) — where the runner fits in the bigger picture
