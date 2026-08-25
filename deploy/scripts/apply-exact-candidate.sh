#!/bin/bash

set -euo pipefail

: "${CANDIDATE_IMAGE_MAP:?CANDIDATE_IMAGE_MAP is required}"
: "${CANDIDATE_RUNNER_IMAGE:?CANDIDATE_RUNNER_IMAGE is required}"
: "${EXPECTED_CANDIDATE_MANIFEST:?EXPECTED_CANDIDATE_MANIFEST is required}"
: "${EXPECTED_CANDIDATE_SHA256:?EXPECTED_CANDIDATE_SHA256 is required}"

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
tmp_manifest="$(mktemp)"
trap 'rm -f "$tmp_manifest"' EXIT

BASELINE_IMAGE_MAP="$CANDIDATE_IMAGE_MAP" \
  BASELINE_RUNNER_IMAGE="$CANDIDATE_RUNNER_IMAGE" \
  "$SCRIPT_DIR/pin-baseline-images.sh" > "$tmp_manifest"

actual_sha256="$(sha256sum "$tmp_manifest" | awk '{print $1}')"
if [ "$actual_sha256" != "$EXPECTED_CANDIDATE_SHA256" ] ||
   ! cmp -s "$EXPECTED_CANDIDATE_MANIFEST" "$tmp_manifest"; then
  echo "ERROR: Helm rendered a candidate different from the exact server-validated manifest; refusing apply." >&2
  diff -u "$EXPECTED_CANDIDATE_MANIFEST" "$tmp_manifest" | head -300 >&2 || true
  exit 42
fi

cat "$tmp_manifest"
