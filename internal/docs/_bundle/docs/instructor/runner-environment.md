# Runner Environment

When a workflow or action runs, the engine spins up (or reuses) a
**runner** — a small Kali pod on the student's VLAN — and executes
your script there. This page documents what's available inside that
runner so you can write scripts that "just work" without hunting
through engine source.

> [!note]
> Everything described here applies to `execution_mode: kali_runner`.
> The `vmware_tools` mode is different — see the bottom of this page.

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
| `CRUCIBLE_TARGET_HOSTNAME` | Target's hostname | `target-01.pod-42.lab` |
| `CRUCIBLE_TARGET_OS` | OS family hint | `linux` / `windows` |
| `CRUCIBLE_POD_ID` | UUID of the student's pod | `c3f1...` |
| `CRUCIBLE_RUN_ID` | UUID of the current assessment run | `9b21...` |
| `CRUCIBLE_WORKFLOW_SLUG` | Slug of the running workflow | `telnet-blocked` |
| `CRUCIBLE_PLAYLIST_SLUG` | Slug of the playlist (if any) | `ssh-hardening-week3` |
| `CRUCIBLE_STUDENT_USERNAME` | The student's Crucible username | `jsmith` |

If a pod has multiple targets, the secondary ones are exposed as
`CRUCIBLE_TARGET_<SLOT>_IP` (e.g. `CRUCIBLE_TARGET_WEB_IP`,
`CRUCIBLE_TARGET_DB_IP`). Slot names come from the blueprint.

---

## Authentication to the target

SSH keys for the default user are pre-installed on the runner. You can
SSH without a password:

```bash
ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 \
    "$CRUCIBLE_TARGET_USERNAME@$CRUCIBLE_TARGET_IP" \
    'whoami'
```

> [!tip]
> Always pass `-o ConnectTimeout=5` (or similar). Without it, a hung
> target can stall your script up to the workflow `timeout_seconds`
> (default 300s) before the engine kills it.

For Windows targets, WinRM is configured with the equivalent of an
auto-login NTLM cred:

```bash
crucible-winrm "$CRUCIBLE_TARGET_IP" "Get-Service WinDefend"
```

(The `crucible-winrm` wrapper is in `/opt/crucible/bin`.)

---

## Pre-installed tools

The runner is Kali Linux 2025.x with the standard pen-testing toolkit,
plus a few Crucible-specific helpers. Highlights:

| Category | Tools |
|---|---|
| Network | `nmap`, `nc`, `netcat`, `tcpdump`, `mtr`, `dig`, `host`, `iperf3` |
| Web | `curl`, `wget`, `httpie`, `gobuster`, `wfuzz` |
| Crypto | `openssl`, `ssh-keygen`, `gpg`, `hashcat` (CPU-only) |
| Parsing | `jq`, `yq`, `xmlstarlet`, `awk`, `sed`, `grep` |
| Crucible helpers | `/opt/crucible/lib/actions.sh`, `/opt/crucible/bin/crucible-winrm`, `/opt/crucible/bin/crucible-context` |

The complete tool list is captured in the runner Dockerfile —
[`deploy/runner/actions.sh`](../../deploy/runner/actions.sh) is the
helper library you `source` at the top of every script.

> [!important]
> If you need a tool that isn't in the runner image, **don't `apt
> install` it from your script** (no internet anyway, and you'd be
> mutating a shared image). File a request and we'll add it to the
> image build.

---

## Filesystem layout

| Path | Contents |
|---|---|
| `/opt/crucible/lib/actions.sh` | Shell helpers (`run_action`, context emit) |
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

> [!warning]
> Use the **smallest reasonable timeout** for each `run_action`. A long
> per-action timeout makes the run feel slow to students and masks real
> hangs. If a check needs more than 30 seconds, ask whether you're
> doing too much in one step.

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
