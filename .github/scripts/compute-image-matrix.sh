#!/bin/bash
# Decide which image matrix components must rebuild for a CI event.
#
# Channels come from dorny/paths-filter via .github/path-filters.yaml.
# Shared channel changes force every component in $all.
#
# Main pushes path-filter like PRs. The Kali runner image lives in
# jmal1/selfservice-crucible-runner and is not part of this matrix.

set -euo pipefail

event_name=${1:?event name is required}
changes=${2:?changed-channel JSON is required}
all=${3:?complete component JSON is required}

if jq -e 'index("shared")' >/dev/null <<< "$changes"; then
  printf '%s\n' "$all"
  exit 0
fi

# Both push and pull_request: rebuild only changed component channels.
# On main, an empty selection means no image rebuild (docs-only / runner-only
# in the external repo). Deploy pins the last runner SHA separately.
jq -c '[.[] | select(. != "shared" and . != "go-tests")]' <<< "$changes"
