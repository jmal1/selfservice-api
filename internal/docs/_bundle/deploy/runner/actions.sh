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
    local prefix
    prefix=$(_name_to_prefix "$name")
    local timeout_sec="${ACTION_TIMEOUT:-30}"

    # Notify sidecar: action starting
    _sidecar_send "{\"event\":\"action_start\",\"action\":\"$name\"}"

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

    # Extract student message from output
    local student_msg=""
    student_msg=$(echo "$output" | grep -oP '(?<=STUDENT_MSG:).*' | tail -1 | xargs) || true

    # Notify sidecar: action complete
    _sidecar_send "{\"event\":\"action_end\",\"action\":\"$name\",\"status\":\"$status\",\"exit_code\":$exit_code,\"duration_ms\":$duration_ms}"

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