#!/bin/bash
# /opt/crucible/lib/actions.sh — sourced by every workflow script
# Provides the run_action wrapper, context management, and action event reporting.

set -euo pipefail

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

    # Execute the action command with timeout
    output=$(timeout "$timeout_sec" "$@" 2>&1) || exit_code=$?

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
