#!/bin/bash

set -euo pipefail

event_name=${1:?event name is required}
changes=${2:?changed-channel JSON is required}
all=${3:?complete component JSON is required}

# Unused today (push and PR both path-filter), but kept so the workflow
# contract and existing callers stay stable.
: "${event_name}" "${all}"

# "shared" forces the Alpine images only. Kali (crucible-runner) rebuilds
# solely when its own path-filter channel is present -- never as a side effect
# of internal/database, models, or other shared Go packages. See Crucible#51.
alpine='["api-gateway","provision-worker","crucible-engine","synthetic-api-monitor"]'

if jq -e 'index("shared")' >/dev/null <<< "$changes"; then
  jq -c --argjson alpine "$alpine" '
    def uniq_keep:
      reduce .[] as $x ([]; if index($x) then . else . + [$x] end);
    ($alpine + [.[] | select(. != "shared" and . != "go-tests")])
    | uniq_keep
  ' <<< "$changes"
else
  jq -c '[.[] | select(. != "shared" and . != "go-tests")]' <<< "$changes"
fi
