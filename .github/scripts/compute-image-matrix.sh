#!/bin/bash

set -euo pipefail

event_name=${1:?event name is required}
changes=${2:?changed-channel JSON is required}
all=${3:?complete component JSON is required}

if [ "$event_name" = push ]; then
  printf '%s\n' "$all"
elif jq -e 'index("shared")' >/dev/null <<< "$changes"; then
  printf '%s\n' "$all"
else
  jq -c '[.[] | select(. != "shared" and . != "go-tests")]' <<< "$changes"
fi
