#!/bin/bash
# /opt/crucible/lib/actions.sh — sourced by every workflow script
# Provides the run_action wrapper, context management, and action event reporting.

set -euo pipefail

# Path to this file, exported so run_action's re-entrant subshell can source it.
# Overridable so the contract tests can exercise run_action against the repo copy
# instead of requiring an installed runner image.
export CRUCIBLE_ACTIONS_LIB="${CRUCIBLE_ACTIONS_LIB:-/opt/crucible/lib/actions.sh}"
CRUCIBLE_SOCKET="${CRUCIBLE_SOCKET:-/tmp/crucible-sidecar.sock}"
CRUCIBLE_CONTEXT="${CRUCIBLE_CONTEXT:-/tmp/crucible-context.json}"
SIDECAR_FAILED=false

# Initialize context file if it doesn't exist
[ -f "$CRUCIBLE_CONTEXT" ] || echo '{}' > "$CRUCIBLE_CONTEXT"

# --- Context Management ---

ctx_set() {
    local key="$1" value="$2"
    local tmp
    tmp=$(mktemp)
    jq --arg k "$key" --arg v "$value" '.[$k] = $v' "$CRUCIBLE_CONTEXT" > "$tmp" && mv "$tmp" "$CRUCIBLE_CONTEXT"
}

# Export CTX_<KEY> for every value in the context file.
#
# This is what makes cross-action context passing work at all. The documented
# contract is that an action reads an earlier action's output via
# CTX_<PREFIX>_<FIELD>, and the Go sidecar does build exactly those variables
# (Sidecar.BuildContextEnv) — but the executor only applies them to cmd.Env
# once, before the workflow's single bash process starts, when the context file
# is still `{}`. A process's environment cannot be changed from outside after
# it starts, so a value written by action 1 could never reach action 2. Under
# the `set -euo pipefail` header every workflow is told to use, referencing the
# unset variable aborted the whole workflow.
#
# Doing it here, in the process that owns the context, is the only place it can
# work. Sanitization mirrors sanitizeForEnv in internal/runner/sidecar.go
# (strip NUL, flatten newlines, cap at 4096 bytes) so a value looks the same
# whether it came from the sidecar or from this refresh. Keys are upper-cased
# with every non-alphanumeric turned into `_`, matching the sidecar's
# ToUpper + "."->"_" for the dotted keys run_action actually writes.
_ctx_export() {
    [ -f "$CRUCIBLE_CONTEXT" ] || return 0
    local line envkey
    # One line per entry is guaranteed by the newline flattening below, so
    # splitting on the first `=` is unambiguous even for a value containing one.
    while IFS= read -r line; do
        envkey="${line%%=*}"
        [ -n "$envkey" ] || continue
        export "CTX_$envkey=${line#*=}"
    done < <(jq -r '
        to_entries[]
        | (.key | ascii_upcase | gsub("[^A-Z0-9]"; "_")) as $k
        | ((.value | tostring)
            | gsub("\u0000"; "")
            | gsub("[\n\r]"; " ")
            | .[0:4096]) as $v
        | "\($k)=\($v)"
    ' "$CRUCIBLE_CONTEXT" 2>/dev/null)
}

# --- Target access ---

# crucible_ssh <argv...> — run a command on the assessment target.
#
# This exists because "check the student's VM" had no implementation. Actions
# like service-running and package-installed ran `systemctl` and `dpkg`
# directly, which inside a kali_runner workflow inspects the RUNNER POD, not
# the student's machine — an assessment that is green or red for reasons the
# student cannot influence either way. Every target-side action now goes
# through here.
#
# Arguments are quoted with `printf %q` before being handed to ssh, because ssh
# concatenates its command arguments with spaces and lets the REMOTE shell
# re-parse the result. Passing a value containing a space or a metacharacter
# unquoted would silently split it, or execute it. To deliberately run remote
# shell syntax, pass it as an explicit `bash -c '<script>'`, which quotes
# correctly through the same path.
#
# Auth order is key first, password second. A key is the better credential and
# is used whenever CRUCIBLE_SSH_KEY points at a readable file; otherwise the
# generated guest password the engine already injects is spent through sshpass.
# The password goes via the SSHPASS environment variable rather than `-p`,
# because a `-p` argument is visible in the target's process list to any user
# on the box — including the student being assessed.
#
# StrictHostKeyChecking is off and known_hosts is /dev/null: every pod VM is a
# fresh clone with a fresh host key, so there is nothing to pin and a persisted
# entry would make the second run of an assessment fail with a host-key warning.
crucible_ssh() {
    local host="${CRUCIBLE_TARGET_IP:-}"
    local user="${CRUCIBLE_TARGET_USERNAME:-}"
    if [ -z "$host" ] || [ -z "$user" ]; then
        LAST_ERROR="crucible_ssh: CRUCIBLE_TARGET_IP and CRUCIBLE_TARGET_USERNAME must both be set"
        return 78
    fi

    local remote
    printf -v remote '%q ' "$@"

    local opts=(
        -o StrictHostKeyChecking=no
        -o UserKnownHostsFile=/dev/null
        -o ConnectTimeout=5
        -o LogLevel=ERROR
    )

    if [ -n "${CRUCIBLE_SSH_KEY:-}" ] && [ -r "${CRUCIBLE_SSH_KEY}" ]; then
        ssh -i "$CRUCIBLE_SSH_KEY" -o BatchMode=yes "${opts[@]}" "${user}@${host}" "$remote"
        return $?
    fi
    if [ -n "${CRUCIBLE_TARGET_PASSWORD:-}" ]; then
        SSHPASS="$CRUCIBLE_TARGET_PASSWORD" sshpass -e ssh \
            -o PreferredAuthentications=password \
            -o PubkeyAuthentication=no \
            "${opts[@]}" "${user}@${host}" "$remote"
        return $?
    fi
    LAST_ERROR="crucible_ssh: no CRUCIBLE_SSH_KEY and no CRUCIBLE_TARGET_PASSWORD; the target has no usable credential"
    return 78
}

# crucible_ssh_sudo <argv...> — same, but elevated and non-interactive.
#
# `sudo -n` never prompts. If the student removed their own sudo access the
# command fails immediately instead of hanging until the action times out,
# which turns a 30-second stall into an explainable error.
crucible_ssh_sudo() {
    crucible_ssh sudo -n "$@"
}

ctx_get() {
    # ctx_get is for reading within the same action only.
    # NEVER use in command interpolation $(ctx_get ...) — use CTX_* env vars instead.
    # The Go sidecar injects sanitized CTX_<PREFIX>_<FIELD> env vars for cross-action references.
    local raw
    raw=$(jq -r --arg k "$1" '.[$k] // empty' "$CRUCIBLE_CONTEXT")
    # Escape shell metacharacters to prevent injection if accidentally interpolated
    printf '%s' "$raw" | sed 's/[;&|`$(){}!\]/\\&/g'
}

# --- Internal Helpers ---

# Convert action name to snake_case prefix
_name_to_prefix() {
    echo "$1" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9]/_/g' | sed 's/__*/_/g' | sed 's/^_//;s/_$//'
}

# Escape a string for embedding in a JSON string literal.
#
# Event payloads used to be built by splicing shell variables straight into a
# JSON template. That is fine until a value contains a quote, a backslash or a
# newline — and the student message, which is derived from an action's output,
# routinely can. A malformed payload does not error: the sidecar fails to
# decode it, logs at debug level, and the action's result silently never
# arrives, so the workflow reports fewer actions than it ran.
#
# Done with parameter expansion rather than jq deliberately. jq is installed and
# build-verified in the runner image, but the event path is how results reach
# the engine at all; making it depend on an external binary means that if the
# binary is ever missing the failure mode is total, silent result loss. Backslash
# must be substituted first, or it would double-escape the escapes added after it.
_json_escape() {
    local s="${1-}"
    s="${s//\\/\\\\}"
    s="${s//\"/\\\"}"
    s="${s//$'\n'/\\n}"
    s="${s//$'\r'/\\r}"
    s="${s//$'\t'/\\t}"
    printf '%s' "$s"
}

# Send event to sidecar via Unix socket
_sidecar_send() {
    local payload="$1"
    if [ "$SIDECAR_FAILED" = "true" ]; then
        echo "$payload" >> /tmp/crucible-fallback-results.json
        return 0
    fi

    if ! echo "$payload" | socat - UNIX-CONNECT:"$CRUCIBLE_SOCKET" 2>/dev/null; then
        SIDECAR_FAILED=true
        echo "$payload" >> /tmp/crucible-fallback-results.json
    fi
}

# --- Main Action Wrapper ---

run_action() {
    local name="$1"
    shift
    local cmd="$1"
    local prefix
    prefix=$(_name_to_prefix "$name")
    local timeout_sec="${ACTION_TIMEOUT:-30}"

    # Notify sidecar: action starting
    _sidecar_send "{\"event\":\"action_start\",\"action\":\"$(_json_escape "$name")\"}"

    local start_time exit_code output
    start_time=$(date +%s%N)
    exit_code=0

    # Execute the action command with timeout.
    #
    # `timeout` is an external binary: it execve()s its argument, so it CANNOT
    # run a shell function. Library actions (http_get, port_open, ssh_exec, …)
    # are shell functions, so dispatching them through plain `timeout` fails with
    #   timeout: failed to run command 'http_get': No such file or directory
    # and exit code 127 — a "failure" that looks like a student misconfiguration
    # but is really the runner being unable to call its own action library.
    #
    # For a function we therefore re-enter bash inside the timeout and re-source
    # this file, which pulls in ctx_set/ctx_get and (via the guard at the bottom)
    # the generated library. Re-sourcing rather than `export -f` avoids having to
    # keep an export list in sync with whatever the engine injected.
    #
    # `set +e` in the subshell is deliberate. Action bodies are written to detect
    # their own failures and set LAST_ERROR/LAST_STUDENT_MSG before `return 1`;
    # under the inherited `set -e` the first non-zero command (a curl that cannot
    # connect, say) would abort the body before it could produce that message,
    # turning an actionable "Web server returned 000 instead of 200" into a bare
    # non-zero exit.
    #
    # Context survives the subshell because ctx_set writes to $CRUCIBLE_CONTEXT
    # on disk, not to shell state.
    # The trailing emit of LAST_STUDENT_MSG/LAST_ERROR is load-bearing. Library
    # action bodies report problems by assigning those two variables, but
    # run_action harvests the student-facing message by grepping stdout for
    # "STUDENT_MSG:" — and the body runs in a command-substitution subshell, so a
    # plain variable assignment can never reach the caller. Without this, all 18
    # library actions would fail with a correct exit code and no explanation at
    # all, which is the difference between "Port 8080 is not responding on
    # 10.100.19.10. Is the service running?" and a bare red X.
    if declare -F "$1" >/dev/null 2>&1; then
        output=$(timeout "$timeout_sec" bash -c '
            source "$CRUCIBLE_ACTIONS_LIB"
            set +e
            "$@"
            _rc=$?
            [ -n "${LAST_STUDENT_MSG:-}" ] && echo "STUDENT_MSG:$LAST_STUDENT_MSG"
            [ -n "${LAST_ERROR:-}" ] && echo "ERROR:$LAST_ERROR"
            exit $_rc
        ' crucible-action "$@" 2>&1) || exit_code=$?
    else
        output=$(timeout "$timeout_sec" "$@" 2>&1) || exit_code=$?
    fi

    local end_time duration_ms
    end_time=$(date +%s%N)
    duration_ms=$(( (end_time - start_time) / 1000000 ))

    # Handle timeout specifically
    local status="pass"
    if [ "$exit_code" -eq 124 ]; then
        status="timeout"
    elif [ "$exit_code" -ne 0 ]; then
        status="fail"
    fi

    # Auto-save outputs to context
    ctx_set "${prefix}.status" "$exit_code"
    ctx_set "${prefix}.body" "$output"

    # Auto-parse JSON fields if output is valid JSON
    if echo "$output" | jq empty 2>/dev/null; then
        for field in id url slug name; do
            local val
            val=$(echo "$output" | jq -r ".$field // empty" 2>/dev/null) || true
            [ -n "$val" ] && ctx_set "${prefix}.${field}" "$val"
        done
    fi

    # Publish everything this action wrote as CTX_* for whatever runs next.
    # Done after the writes above rather than before the dispatch so the values
    # are visible both to the next run_action and to plain shell code between
    # actions in the workflow body.
    _ctx_export

    # Extract the student-facing message the action produced.
    #
    # Library action bodies report problems by setting LAST_STUDENT_MSG, which
    # the dispatch above re-emits on stdout as "STUDENT_MSG:...". Hand-written
    # workflow scripts echo the same marker directly. Either way it ends up in
    # "$output", and it must be attached to the event below — NOT left for the
    # executor to scrape out of the workflow's stdout. The socket event and the
    # stdout pipe are independent channels, and this event is necessarily sent
    # before the output is echoed, so a stdout-only harvest races the Go reader
    # and loses the message.
    #
    # Trimming is done with sed, not `xargs`. xargs does shell-style word
    # splitting: it strips quotes and treats backslashes as escapes, so
    #   Expected "200" but got C:\path
    # arrives as
    #   Expected 200 but got C:path
    # and an odd number of quotes makes xargs fail outright with "unmatched
    # double quote", losing the message entirely. Student messages routinely
    # quote expected values and Windows paths.
    local student_msg=""
    student_msg=$(echo "$output" | grep -oP '(?<=STUDENT_MSG:).*' | tail -1 \
        | sed 's/^[[:space:]]*//; s/[[:space:]]*$//') || true

    # Exit 127 means "command not found", and it is the single most misleading
    # status this runner can produce. To a student it is indistinguishable from
    # "your service is misconfigured": the check went red, with no explanation.
    # In reality the workflow asked for a tool that is not in the runner image,
    # which is an authoring/infrastructure defect and nothing the student can
    # fix by changing their VM.
    #
    # We only synthesize a message when the action produced none of its own —
    # a library action that legitimately exits 127 from an inner command has
    # already said something more specific, and overwriting that would trade a
    # precise message for a generic one.
    #
    # The authoring-time counterpart is CRU0002 in internal/scriptvalidator,
    # which warns the instructor before the workflow is ever saved. This branch
    # is the runtime backstop for the cases static analysis cannot see
    # (dynamically constructed command names, tools removed from the image
    # after a workflow was published).
    if [ "$exit_code" -eq 127 ] && [ -z "$student_msg" ]; then
        status="error"
        if declare -F "$cmd" >/dev/null 2>&1; then
            # `cmd` is a library action (a shell function), so the missing
            # binary is somewhere inside its body. Naming `cmd` here would
            # point the instructor at a function that exists.
            student_msg="This check could not run because a tool it depends on is not available in the assessment runner. This is a problem with the assessment itself, not with your work — please report it to your instructor."
            output="${output}
ERROR:command not found (exit 127) inside library action '${cmd}'. A command it invokes is not installed in the runner image. Check the action body against internal/runnertools/tools.txt."
        else
            student_msg="This check could not run: the command '${cmd}' is not available in the assessment runner. This is a problem with the assessment itself, not with your work — please report it to your instructor."
            output="${output}
ERROR:command not found: ${cmd} (exit 127). '${cmd}' is not installed in the runner image. Add it to internal/runnertools/tools.txt and rebuild, or use a tool that is already present."
        fi
    fi

    # Notify sidecar: action complete
    _sidecar_send "{\"event\":\"action_end\",\"action\":\"$(_json_escape "$name")\",\"status\":\"$status\",\"message\":\"$(_json_escape "$student_msg")\",\"exit_code\":$exit_code,\"duration_ms\":$duration_ms}"

    # Print output for debugging (visible in instructor_output)
    if [ -n "$output" ]; then
        echo "$output"
    fi

    return $exit_code
}

# ---------------------------------------------------------------------------
# Action library
#
# The engine generates /opt/crucible/lib/library.sh per run from the library
# actions in the database and the runner materialises it before executing any
# workflow. Sourcing it here is what makes `run_action "..." http_get ...`
# resolve; without it every library action dies with exit 127.
#
# This MUST be an `if` block, not `[ -f x ] && source x`. This file runs under
# `set -euo pipefail`, and a trailing `&&` chain whose test fails returns a
# non-zero status from the last command, which aborts the sourcing script and
# takes the whole workflow down whenever the library happens to be absent. An
# `if` with no `else` branch returns 0.
# ---------------------------------------------------------------------------
if [ -f "${CRUCIBLE_ACTION_LIBRARY:-/opt/crucible/lib/library.sh}" ]; then
    # shellcheck source=/dev/null
    source "${CRUCIBLE_ACTION_LIBRARY:-/opt/crucible/lib/library.sh}"
fi