#!/bin/bash
# Deploy the selfservice helm chart to the production K3s cluster.
#
# This script is intended to run on k3sv01 from a sparse-checkout of this
# repository where ./deploy/helm/selfservice is the chart root.
#
# Usage:
#   ./deploy.sh --ui-source-sha <full-sha>
#   ./deploy.sh --no-pull --ui-source-sha <full-sha>
#   ./deploy.sh --dry-run --ui-source-sha <full-sha>
#   ./deploy.sh --verify-rollback-containment
#   ./deploy.sh --prepare-claims-baseline --baseline-chart-dir /path/to/safe/chart
#
# Required on the deploy host:
#   - kubectl with KUBECONFIG pointing at k3s (typically /etc/rancher/k3s/k3s.yaml)
#   - helm 3.14+
#   - jq
#   - curl
#   - tar
#   - unzip
#   - gh authenticated for source workflow artifacts and private GHCR packages
#   - SSH key (deploy key) allowing `git pull` from the repo

set -euo pipefail

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
CHART_DIR="$SCRIPT_DIR/../helm/selfservice"
RELEASE=selfservice
NAMESPACE=selfservice
TIMEOUT=5m

DO_PULL=true
DO_DRY_RUN=false
MODE=deploy
BASELINE_CHART_DIR=
UI_SOURCE_REPOSITORY=jmal1/selfservice-ui
UI_SOURCE_WORKFLOW=ci.yaml
UI_SOURCE_SHA=

while [ "$#" -gt 0 ]; do
  case "$1" in
    --no-pull) DO_PULL=false ;;
    --dry-run) DO_DRY_RUN=true ;;
    --verify-rollback-containment)
      MODE=verify
      DO_PULL=false
      ;;
    --prepare-claims-baseline)
      MODE=prepare
      DO_PULL=false
      ;;
    --baseline-chart-dir)
      shift
      [ "$#" -gt 0 ] || { echo "ERROR: --baseline-chart-dir requires a path" >&2; exit 64; }
      BASELINE_CHART_DIR=$1
      ;;
    --ui-source-repo)
      shift
      [ "$#" -gt 0 ] || { echo "ERROR: --ui-source-repo requires owner/repository" >&2; exit 64; }
      UI_SOURCE_REPOSITORY=$1
      ;;
    --ui-source-workflow)
      shift
      [ "$#" -gt 0 ] || { echo "ERROR: --ui-source-workflow requires a workflow file" >&2; exit 64; }
      UI_SOURCE_WORKFLOW=$1
      ;;
    --ui-source-sha)
      shift
      [ "$#" -gt 0 ] || { echo "ERROR: --ui-source-sha requires a full commit SHA" >&2; exit 64; }
      UI_SOURCE_SHA=$1
      ;;
    *) echo "Unknown arg: $1" >&2; exit 64 ;;
  esac
  shift
done

if [ -z "${KUBECONFIG:-}" ]; then
  if [ -f /etc/rancher/k3s/k3s.yaml ]; then
    export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  fi
fi

PIN_BASELINE_SCRIPT="$SCRIPT_DIR/pin-baseline-images.sh"
APPLY_EXACT_CANDIDATE_SCRIPT="$SCRIPT_DIR/apply-exact-candidate.sh"
CANONICALIZE_WORKLOAD_FILTER="$SCRIPT_DIR/canonicalize-workload-spec.jq"
WORKER_REPLICA_FILTER="$SCRIPT_DIR/require-single-worker-replica.jq"
HELM_RELEASE_LOCK="$RELEASE-phase1-deploy-lock"
SOURCE_REPOSITORY=jmal1/selfservice-api
SOURCE_OWNER=${SOURCE_REPOSITORY%%/*}
SOURCE_BRANCH=main
SOURCE_WORKFLOW=ci.yaml
UI_IMAGE_REPOSITORY=ghcr.io/jmal1/selfservice-ui
UI_SOURCE_BRANCH=master
REQUIRED_ROLLBACK_REVISION=163
REPOSITORY_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
HELM_RELEASE_LOCK_HELD=false
HELM_RELEASE_LOCK_HOLDER=
HELM_RELEASE_LOCK_PRESERVE=false
BASELINE_TMP_DIR=

release_helm_release_lock() {
  if [ "$HELM_RELEASE_LOCK_HELD" != true ]; then
    return
  fi
  local live_holder
  set +e
  live_holder="$(
    kubectl get "configmap/$HELM_RELEASE_LOCK" \
      -n "$NAMESPACE" \
      -o go-template='{{index .metadata.annotations "crucible.jmal.io/holder"}}' \
      2>/dev/null
  )"
  if [ "$live_holder" = "$HELM_RELEASE_LOCK_HOLDER" ]; then
    kubectl delete "configmap/$HELM_RELEASE_LOCK" -n "$NAMESPACE" --wait=true >/dev/null
  else
    echo "WARNING: not deleting Helm release lock $HELM_RELEASE_LOCK because its holder changed to $live_holder." >&2
  fi
  set -e
  HELM_RELEASE_LOCK_HELD=false
}

cleanup_on_exit() {
  local exit_code=$?
  if [ -n "$BASELINE_TMP_DIR" ]; then
    rm -rf "$BASELINE_TMP_DIR"
  fi
  if [ "$HELM_RELEASE_LOCK_PRESERVE" = true ]; then
    echo "WARNING: preserving Helm release lock $HELM_RELEASE_LOCK for manual containment recovery." >&2
  else
    release_helm_release_lock
  fi
  exit "$exit_code"
}

trap cleanup_on_exit EXIT

acquire_helm_release_lock() {
  HELM_RELEASE_LOCK_HOLDER="$(hostname)-$$-$(date -u +%Y%m%dT%H%M%SZ)"
  if ! cat <<EOF | kubectl create -f - >/dev/null
apiVersion: v1
kind: ConfigMap
metadata:
  name: $HELM_RELEASE_LOCK
  namespace: $NAMESPACE
  annotations:
    crucible.jmal.io/holder: "$HELM_RELEASE_LOCK_HOLDER"
    crucible.jmal.io/purpose: "serialize phase-1 Helm baseline and application upgrades"
EOF
  then
    echo "ERROR: Helm release lock configmap/$HELM_RELEASE_LOCK already exists or could not be created." >&2
    kubectl get "configmap/$HELM_RELEASE_LOCK" -n "$NAMESPACE" -o yaml >&2 || true
    echo "Another release mutation may be active. Inspect the holder; never delete this lock without proving it is stale." >&2
    return 1
  fi
  HELM_RELEASE_LOCK_HELD=true
  echo "==> acquired Helm release lock $HELM_RELEASE_LOCK as $HELM_RELEASE_LOCK_HOLDER"
}

claims_from_manifest() {
  awk '
    /^[[:space:]]*- name: WORKER_PROVISIONING_CLAIMS_ENABLED[[:space:]]*$/ {
      matches++
      if (getline <= 0) {
        exit 2
      }
      value = $0
      sub(/^[[:space:]]*value:[[:space:]]*/, "", value)
      gsub(/^["'"'"']|["'"'"']$/, "", value)
    }
    END {
      if (matches != 1 || value == "") {
        exit 3
      }
      print value
    }
  '
}

env_from_manifest() {
  local variable=$1
  awk -v variable="$variable" '
    $0 ~ "^[[:space:]]*- name:[[:space:]]*" variable "[[:space:]]*$" {
      matches++
      match($0, /^[[:space:]]*/)
      indent = RLENGTH
      if ((getline next_line) <= 0) {
        exit 2
      }
      # A server-defaulted dry-run response marshals corev1.EnvVar with
      # `json:"value,omitempty"`, so an empty Value is not rendered as
      # `value: ""` at all -- the line immediately following "- name: X" can
      # instead be the *next* env entrys own "- name:" line, a later sibling
      # field of the *container* (once the env list has no more items), or a
      # YAML document separator. All of those are safe to read as "X was
      # omitted (i.e. empty)" because none of them is a field of Xs own
      # entry. Real k8s-rendered YAML (see the fixtures in this package)
      # aligns a block sequences "- " items with their parent key, so any
      # of those safe shapes sits at the *same or shallower* indent as the
      # "- name: X" line itself; anything indented *deeper* is a field of Xs
      # own entry -- most importantly `valueFrom:` (secretKeyRef /
      # configMapKeyRef), but also any other unrecognized field -- and must
      # never be silently treated as an empty value: a secret- or
      # configMap-sourced variable is not empty just because it has no
      # literal "value:" line.
      if (next_line ~ /^[[:space:]]*value:[[:space:]]*/) {
        value = next_line
        sub(/^[[:space:]]*value:[[:space:]]*/, "", value)
        gsub(/^["'"'"']|["'"'"']$/, "", value)
      } else {
        match(next_line, /^[[:space:]]*/)
        next_indent = RLENGTH
        if (next_indent > indent) {
          exit 4
        }
        value = ""
        # The consumed line is not re-examined by this per-line pattern
        # match, so if it is itself an adjacent "- name: variable" entry at
        # the same indent (immediately-adjacent duplicate with its own
        # value also omitted), it must be re-tested here rather than
        # silently swallowed. Anything else at the same-or-shallower indent
        # (a sibling container field, a further dedent, or "---") simply
        # means Xs entry -- and the env list/block/document containing it
        # -- has ended, with no bearing on duplicate detection.
        if (next_indent == indent && next_line ~ "^[[:space:]]*- name:[[:space:]]*" variable "[[:space:]]*$") {
          matches++
        }
      }
    }
    END {
      if (matches != 1) {
        exit 3
      }
      print value
    }
  '
}

runner_image_from_manifest() {
  awk '
    /^[[:space:]]*- name: RUNNER_IMAGE[[:space:]]*$/ {
      matches++
      if (getline <= 0) {
        exit 2
      }
      value = $0
      sub(/^[[:space:]]*value:[[:space:]]*/, "", value)
      gsub(/^["'"'"']|["'"'"']$/, "", value)
    }
    END {
      if (matches != 1 || value == "") {
        exit 3
      }
      print value
    }
  '
}

rendered_worker_replicas_from_manifest() {
  local manifest=$1
  # extract_workload_manifest returns the whole single-document Deployment
  # YAML, including a server-populated "status:" section when the manifest
  # came from a dry-run apply against an existing live object. "status:"
  # carries its own top-level "  replicas:" field (status.replicas) at the
  # exact same two-space indent as spec.replicas, so a bare
  # "/^  replicas:/" match sees two lines and fails as "duplicate" even
  # though there is exactly one spec.replicas. Track which top-level
  # ("apiVersion:", "kind:", "metadata:", "spec:", "status:", ...) section
  # we are currently in and only accept "  replicas:" while inside "spec:".
  extract_workload_manifest "$manifest" Deployment "$RELEASE-worker" \
    | awk '
        /^[^[:space:]]/ {
          section = $0
          sub(/:.*$/, "", section)
          next
        }
        section == "spec" && /^  replicas:[[:space:]]*/ {
          matches++
          value = $0
          sub(/^  replicas:[[:space:]]*/, "", value)
          gsub(/^["'"'"']|["'"'"']$/, "", value)
        }
        END {
          if (matches != 1 || value == "") {
            exit 3
          }
          print value
        }
      '
}

live_worker_replicas() {
  kubectl get "Deployment/$RELEASE-worker" -n "$NAMESPACE" -o json \
    | jq -er -f "$WORKER_REPLICA_FILTER"
}

synthetic_monitor_suspend_from_manifest() {
  local manifest=$1
  extract_workload_manifest "$manifest" CronJob "$RELEASE-synthetic-api-monitor" \
    | awk '
        /^  suspend:[[:space:]]*/ {
          matches++
          value = $0
          sub(/^  suspend:[[:space:]]*/, "", value)
          gsub(/^["'"'"']|["'"'"']$/, "", value)
        }
        END {
          if (matches != 1 || value == "") {
            exit 3
          }
          print value
        }
      '
}

manifest_workload_inventory() {
  local manifest=$1
  local include_pods=${2:-false}
  awk -v include_pods="$include_pods" '
    function trim_yaml_value(value) {
      sub(/^[[:space:]]*/, "", value)
      sub(/[[:space:]]*$/, "", value)
      gsub(/^["'"'"']|["'"'"']$/, "", value)
      return value
    }

    function is_workload(value) {
      return value == "Deployment" ||
        value == "DaemonSet" ||
        value == "StatefulSet" ||
        value == "CronJob" ||
        value == "Job" ||
        (include_pods == "true" && value == "Pod")
    }

    function finish_container() {
      if (!in_container) {
        return
      }
      if (!is_workload(kind)) {
        in_container = 0
        container_indent = -1
        container_name = ""
        container_image = ""
        return
      }
      containers++
      if (container_name == "" || container_image == "") {
        print "ERROR: missing image inventory for rendered workload container " kind "/" name " " container_type "/" container_name > "/dev/stderr"
        failed = 1
      } else {
        print kind, name, container_type, container_name, container_image
        images++
      }
      in_container = 0
      container_indent = -1
      container_name = ""
      container_image = ""
    }

    function finish_document() {
      finish_container()
      if (!is_workload(kind)) {
        return
      }
      if (name == "" || containers == 0 || containers != images) {
        print "ERROR: missing image inventory for rendered workload " kind "/" name > "/dev/stderr"
        failed = 1
      }
    }

    function reset_document() {
      kind = ""
      name = ""
      top_metadata = 0
      container_type = ""
      section_indent = -1
      in_container = 0
      container_indent = -1
      container_name = ""
      container_image = ""
      containers = 0
      images = 0
    }

    BEGIN {
      OFS = "\t"
      reset_document()
    }

    /^---[[:space:]]*$/ {
      finish_document()
      reset_document()
      next
    }

    /^kind:[[:space:]]*/ {
      kind = $0
      sub(/^kind:[[:space:]]*/, "", kind)
      kind = trim_yaml_value(kind)
    }

    /^metadata:[[:space:]]*$/ {
      top_metadata = 1
    }

    top_metadata && /^  name:[[:space:]]*/ {
      name = $0
      sub(/^  name:[[:space:]]*/, "", name)
      name = trim_yaml_value(name)
      top_metadata = 0
    }

    top_metadata && /^[^[:space:]]/ && !/^metadata:[[:space:]]*$/ {
      top_metadata = 0
    }

    {
      line = $0
      match(line, /^[[:space:]]*/)
      indent = RLENGTH
      stripped = substr(line, indent + 1)

      if (stripped ~ /^(initContainers|containers):[[:space:]]*$/) {
        finish_container()
        container_type = stripped
        sub(/:.*/, "", container_type)
        section_indent = indent
      } else if (container_type != "" && ((!in_container && (indent == section_indent || indent == section_indent + 2)) || (in_container && indent == container_indent)) && stripped ~ /^- /) {
        finish_container()
        in_container = 1
        container_indent = indent
        field = stripped
        sub(/^- /, "", field)
        if (field ~ /^name:[[:space:]]*/) {
          sub(/^name:[[:space:]]*/, "", field)
          container_name = trim_yaml_value(field)
        } else if (field ~ /^image:[[:space:]]*/) {
          sub(/^image:[[:space:]]*/, "", field)
          container_image = trim_yaml_value(field)
        }
      } else if (container_type != "" && in_container && indent == container_indent + 2 && stripped ~ /^name:[[:space:]]*/) {
        field = stripped
        sub(/^name:[[:space:]]*/, "", field)
        container_name = trim_yaml_value(field)
      } else if (container_type != "" && in_container && indent == container_indent + 2 && stripped ~ /^image:[[:space:]]*/) {
        field = stripped
        sub(/^image:[[:space:]]*/, "", field)
        container_image = trim_yaml_value(field)
      } else if (container_type != "" && stripped != "" && indent <= section_indent) {
        finish_container()
        container_type = ""
        section_indent = -1
      }
    }

    END {
      finish_document()
      if (failed) {
        exit 3
      }
    }
  ' "$manifest"
}

extract_workload_manifest() {
  local manifest=$1
  local wanted_kind=$2
  local wanted_name=$3
  awk -v wanted_kind="$wanted_kind" -v wanted_name="$wanted_name" '
    function trim_yaml_value(value) {
      sub(/^[[:space:]]*/, "", value)
      sub(/[[:space:]]*$/, "", value)
      gsub(/^["'"'"']|["'"'"']$/, "", value)
      return value
    }

    function finish_document() {
      if (kind == wanted_kind && name == wanted_name) {
        printf "%s", document
        matches++
      }
    }

    function reset_document() {
      document = ""
      kind = ""
      name = ""
      top_metadata = 0
    }

    BEGIN {
      reset_document()
    }

    /^---[[:space:]]*$/ {
      finish_document()
      reset_document()
    }

    {
      document = document $0 ORS
    }

    /^kind:[[:space:]]*/ {
      kind = $0
      sub(/^kind:[[:space:]]*/, "", kind)
      kind = trim_yaml_value(kind)
    }

    /^metadata:[[:space:]]*$/ {
      top_metadata = 1
    }

    top_metadata && /^  name:[[:space:]]*/ {
      name = $0
      sub(/^  name:[[:space:]]*/, "", name)
      name = trim_yaml_value(name)
      top_metadata = 0
    }

    END {
      finish_document()
      if (matches != 1) {
        exit 3
      }
    }
  ' "$manifest"
}

image_repository() {
  local image=$1
  image=${image%%@*}
  local prefix=${image%/*}
  local leaf=${image##*/}
  if [[ "$leaf" == *:* ]]; then
    leaf=${leaf%%:*}
  fi
  if [ "$prefix" = "$image" ]; then
    printf '%s' "$leaf"
  else
    printf '%s/%s' "$prefix" "$leaf"
  fi
}

canonical_image_repository() {
  local repository
  repository="$(image_repository "$1")"
  local first_segment=${repository%%/*}
  if [ "$first_segment" = "$repository" ]; then
    printf 'docker.io/library/%s' "$repository"
  elif [[ "$first_segment" != *.* && "$first_segment" != *:* && "$first_segment" != "localhost" ]]; then
    printf 'docker.io/%s' "$repository"
  else
    printf '%s' "$repository"
  fi
}

is_digest_image() {
  [[ "$1" =~ ^[^[:space:]]+@sha256:[0-9a-fA-F]{64}$ ]]
}

verify_image_revision() (
  set -euo pipefail
  local image=$1
  local source_sha=$2
  local repository digest repository_path github_token tmp_dir basic_config bearer_config
  local token_json registry_token manifest media_type child_digest config_digest config revision
  repository="$(canonical_image_repository "$image")"
  digest=${image##*@}
  repository_path=${repository#ghcr.io/}
  if [[ "$repository" != ghcr.io/* ]] || [[ ! "$digest" =~ ^sha256:[0-9a-fA-F]{64}$ ]]; then
    echo "ERROR: revision proof requires an immutable ghcr.io image, got $image." >&2
    return 1
  fi
  if ! command -v curl >/dev/null 2>&1; then
    echo "ERROR: curl is required to verify OCI image revision labels." >&2
    return 1
  fi
  github_token="$(gh auth token)"
  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' EXIT
  basic_config="$tmp_dir/github-auth.curl"
  bearer_config="$tmp_dir/registry-auth.curl"
  umask 077
  printf 'user = "%s:%s"\n' "$SOURCE_OWNER" "$github_token" > "$basic_config"
  token_json="$(
    curl \
      --config "$basic_config" \
      --fail \
      --silent \
      --show-error \
      --get \
      --data-urlencode service=ghcr.io \
      --data-urlencode "scope=repository:$repository_path:pull" \
      https://ghcr.io/token
  )"
  registry_token="$(jq -er '.token' <<< "$token_json")"
  printf 'header = "Authorization: Bearer %s"\n' "$registry_token" > "$bearer_config"
  manifest="$(
    curl \
      --config "$bearer_config" \
      --fail \
      --silent \
      --show-error \
      --header 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
      "https://ghcr.io/v2/$repository_path/manifests/$digest"
  )"
  media_type="$(jq -er '.mediaType' <<< "$manifest")"
  case "$media_type" in
    application/vnd.oci.image.index.v1+json|application/vnd.docker.distribution.manifest.list.v2+json)
      child_digest="$(
        jq -er '
          [
            .manifests[]
            | select(.platform.os == "linux" and .platform.architecture == "amd64")
            | .digest
          ]
          | if length == 1 then .[0] else error("expected one linux/amd64 image manifest") end
        ' <<< "$manifest"
      )"
      manifest="$(
        curl \
          --config "$bearer_config" \
          --fail \
          --silent \
          --show-error \
          --header 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
          "https://ghcr.io/v2/$repository_path/manifests/$child_digest"
      )"
      ;;
    application/vnd.oci.image.manifest.v1+json|application/vnd.docker.distribution.manifest.v2+json)
      ;;
    *)
      echo "ERROR: unsupported OCI manifest media type $media_type for $image." >&2
      return 1
      ;;
  esac
  config_digest="$(jq -er '.config.digest' <<< "$manifest")"
  # GHCR serves blob bytes from a redirect (typically to Azure Blob Storage),
  # not from ghcr.io itself, so this request MUST follow the redirect or it
  # silently receives an empty body instead of the runtime config. `--location`
  # alone is sufficient and safe here: curl only forwards the bearer
  # Authorization header (from --config "$bearer_config") to the initial
  # ghcr.io host and drops it on any cross-host redirect, so the blob-storage
  # backend never sees the GHCR token. `--proto`/`--proto-redir` pin both the
  # initial request and the redirect target to https so a compromised or
  # misbehaving redirect can't downgrade the transfer.
  config="$(
    curl \
      --config "$bearer_config" \
      --fail \
      --silent \
      --show-error \
      --location \
      --proto '=https' \
      --proto-redir '=https' \
      "https://ghcr.io/v2/$repository_path/blobs/$config_digest"
  )"
  revision="$(jq -er '.config.Labels["org.opencontainers.image.revision"]' <<< "$config")"
  if [ "$revision" != "$source_sha" ]; then
    echo "ERROR: OCI image $image declares revision $revision, not source commit $source_sha." >&2
    return 1
  fi
)

require_clean_source_tree() {
  local status
  status="$(git -C "$REPOSITORY_ROOT" status --porcelain --untracked-files=normal)"
  if [ -n "$status" ]; then
    echo "ERROR: source tree is dirty; production deploys require an exact committed checkout." >&2
    printf '%s\n' "$status" >&2
    return 1
  fi
}

require_trusted_source_identity() {
  local expected_sha=${1:-}
  local branch head remote_head remote_url
  require_clean_source_tree
  if ! branch="$(git -C "$REPOSITORY_ROOT" symbolic-ref --quiet --short HEAD)"; then
    echo "ERROR: detached HEAD is not a trusted production source." >&2
    return 1
  fi
  if [ "$branch" != "$SOURCE_BRANCH" ]; then
    echo "ERROR: production source branch is $branch, not $SOURCE_BRANCH." >&2
    return 1
  fi
  head="$(git -C "$REPOSITORY_ROOT" rev-parse HEAD)"
  remote_head="$(git -C "$REPOSITORY_ROOT" rev-parse "refs/remotes/origin/$SOURCE_BRANCH")"
  if [ "$head" != "$remote_head" ]; then
    echo "ERROR: checked-out commit $head does not match origin/$SOURCE_BRANCH at $remote_head." >&2
    return 1
  fi
  if [ -n "$expected_sha" ] && [ "$head" != "$expected_sha" ]; then
    echo "ERROR: source commit changed from proven commit $expected_sha to $head." >&2
    return 1
  fi
  remote_url="$(git -C "$REPOSITORY_ROOT" remote get-url origin)"
  case "$remote_url" in
    "https://github.com/$SOURCE_REPOSITORY"|"https://github.com/$SOURCE_REPOSITORY.git"|\
    "git@github.com:$SOURCE_REPOSITORY"|"git@github.com:$SOURCE_REPOSITORY.git"|\
    "ssh://git@github.com/$SOURCE_REPOSITORY"|"ssh://git@github.com/$SOURCE_REPOSITORY.git")
      ;;
    *)
      echo "ERROR: origin $remote_url is not the trusted github.com/$SOURCE_REPOSITORY repository." >&2
      return 1
      ;;
  esac
  printf '%s' "$head"
}

prove_successful_source_build() {
  local source_sha=$1
  local commit_json runs_json run_id jobs_json component
  if ! command -v gh >/dev/null 2>&1; then
    echo "ERROR: gh is required to prove the source commit and image workflow." >&2
    return 1
  fi
  commit_json="$(gh api "repos/$SOURCE_REPOSITORY/commits/$source_sha")"
  if ! jq -e --arg sha "$source_sha" \
      '.sha == $sha and .commit.verification.verified == true' \
      >/dev/null <<< "$commit_json"; then
    echo "ERROR: source commit $source_sha is missing or is not verified by GitHub." >&2
    return 1
  fi
  runs_json="$(
    gh api \
      "repos/$SOURCE_REPOSITORY/actions/workflows/$SOURCE_WORKFLOW/runs?head_sha=$source_sha&event=push&status=success&per_page=100"
  )"
  if ! run_id="$(
    jq -er \
      --arg sha "$source_sha" \
      --arg branch "$SOURCE_BRANCH" \
      '[
        .workflow_runs[]
        | select(
            .head_sha == $sha and
            .head_branch == $branch and
            .event == "push" and
            .status == "completed" and
            .conclusion == "success"
          )
      ] | sort_by(.run_number) | last | .id' \
      <<< "$runs_json"
  )"; then
    echo "ERROR: no successful completed $SOURCE_WORKFLOW push run exists for exact commit $source_sha." >&2
    return 1
  fi
  jobs_json="$(
    gh api "repos/$SOURCE_REPOSITORY/actions/runs/$run_id/jobs?per_page=100"
  )"
  for component in \
    api-gateway \
    provision-worker \
    crucible-engine \
    synthetic-api-monitor \
    crucible-runner; do
    if ! jq -e --arg job "build ($component)" \
        '.jobs | any(.name == $job and .status == "completed" and .conclusion == "success")' \
        >/dev/null <<< "$jobs_json"; then
      echo "ERROR: successful workflow run $run_id does not contain a successful image build for $component." >&2
      return 1
    fi
  done
  PROVEN_WORKFLOW_RUN_ID=$run_id
}

prove_successful_ui_build() {
  local source_sha=$1
  local commit_json runs_json run_id run_attempt jobs_json job
  if [ "$UI_SOURCE_REPOSITORY" != "jmal1/selfservice-ui" ] ||
     [ "$UI_SOURCE_WORKFLOW" != "ci.yaml" ]; then
    echo "ERROR: the production UI source must be jmal1/selfservice-ui workflow ci.yaml." >&2
    return 1
  fi
  if [[ ! "$source_sha" =~ ^[0-9a-f]{40}$ ]]; then
    echo "ERROR: --ui-source-sha must be a full lowercase 40-character commit SHA." >&2
    return 1
  fi
  commit_json="$(gh api "repos/$UI_SOURCE_REPOSITORY/commits/$source_sha")"
  if ! jq -e --arg sha "$source_sha" \
      '.sha == $sha and .commit.verification.verified == true' \
      >/dev/null <<< "$commit_json"; then
    echo "ERROR: UI source commit $source_sha is missing or is not verified by GitHub." >&2
    return 1
  fi
  runs_json="$(
    gh api \
      "repos/$UI_SOURCE_REPOSITORY/actions/workflows/$UI_SOURCE_WORKFLOW/runs?head_sha=$source_sha&event=push&status=success&per_page=100"
  )"
  if ! IFS=$'\t' read -r run_id run_attempt < <(
    jq -er \
      --arg sha "$source_sha" \
      --arg branch "$UI_SOURCE_BRANCH" \
      '[
        .workflow_runs[]
        | select(
            .head_sha == $sha and
            .head_branch == $branch and
            .event == "push" and
            .status == "completed" and
            .conclusion == "success"
          )
      ]
      | sort_by(.run_number)
      | last
      | [.id, .run_attempt]
      | @tsv' \
      <<< "$runs_json"
  ); then
    echo "ERROR: no successful completed UI push run exists for exact commit $source_sha." >&2
    return 1
  fi
  jobs_json="$(
    gh api "repos/$UI_SOURCE_REPOSITORY/actions/runs/$run_id/jobs?per_page=100"
  )"
  for job in test build; do
    if ! jq -e --arg job "$job" \
        '.jobs | any(.name == $job and .status == "completed" and .conclusion == "success")' \
        >/dev/null <<< "$jobs_json"; then
      echo "ERROR: successful UI workflow run $run_id does not contain a successful $job job." >&2
      return 1
    fi
  done
  PROVEN_UI_WORKFLOW_RUN_ID=$run_id
  PROVEN_UI_WORKFLOW_RUN_ATTEMPT=$run_attempt
}

read_build_record_blob() {
  local record=$1
  local digest=$2
  if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "ERROR: UI build record contains invalid OCI digest $digest." >&2
    return 1
  fi
  tar -xOf "$record" "blobs/sha256/${digest#sha256:}"
}

resolve_ui_candidate_image() (
  set -euo pipefail
  local source_sha=$1
  local run_id=$2
  local run_attempt=$3
  local artifacts_json artifact_row artifact_id artifact_name
  local tmp_dir download record magic zip_entries zip_entry
  local index_json manifest_digest manifest_json config_digest config_json
  local result_digest expected_builder expected_tag image
  artifacts_json="$(
    gh api "repos/$UI_SOURCE_REPOSITORY/actions/runs/$run_id/artifacts?per_page=100"
  )"
  artifact_row="$(
    jq -er '
      [
        .artifacts[]
        | select((.expired | not) and (.name | endswith(".dockerbuild")))
        | [.id, .name]
      ]
      | if length == 1 then .[0] else error("expected exactly one unexpired Buildx build record") end
      | @tsv
    ' <<< "$artifacts_json"
  )" || {
    echo "ERROR: UI workflow run $run_id does not have exactly one unexpired Buildx build record." >&2
    return 1
  }
  IFS=$'\t' read -r artifact_id artifact_name <<< "$artifact_row"
  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' EXIT
  download="$tmp_dir/artifact-download"
  record="$tmp_dir/$artifact_name"
  if ! gh api "repos/$UI_SOURCE_REPOSITORY/actions/artifacts/$artifact_id/zip" > "$download"; then
    echo "ERROR: could not download UI build record $artifact_name from workflow run $run_id." >&2
    return 1
  fi
  magic="$(od -An -tx1 -N4 "$download" | tr -d ' \n')"
  if [[ "$magic" == 504b0304* ]]; then
    if ! command -v unzip >/dev/null 2>&1; then
      echo "ERROR: unzip is required to read GitHub workflow artifact archives." >&2
      return 1
    fi
    zip_entries="$(unzip -Z1 "$download")"
    zip_entry="$(
      printf '%s\n' "$zip_entries" \
        | awk '/^[A-Za-z0-9._~\/-]+[.]dockerbuild$/ && $0 !~ /(^|\/)[.][.](\/|$)/ { print }'
    )"
    if [ "$(printf '%s\n' "$zip_entry" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' ')" != "1" ] ||
       ! unzip -p "$download" "$zip_entry" > "$record"; then
      echo "ERROR: UI workflow artifact must contain exactly one safe .dockerbuild record." >&2
      return 1
    fi
  else
    cp "$download" "$record"
  fi
  if ! tar -tf "$record" >/dev/null; then
    echo "ERROR: could not download or read UI build record $artifact_name from workflow run $run_id." >&2
    return 1
  fi
  index_json="$(tar -xOf "$record" index.json)"
  manifest_digest="$(
    jq -er '
      if (.manifests | length) == 1
      then .manifests[0].digest
      else error("expected one build record manifest")
      end
    ' <<< "$index_json"
  )"
  manifest_json="$(read_build_record_blob "$record" "$manifest_digest")"
  config_digest="$(jq -er '.config.digest' <<< "$manifest_json")"
  config_json="$(read_build_record_blob "$record" "$config_digest")"
  result_digest="$(
    jq -er '
      (.Result.Results | to_entries | map(.value.digest) | unique) as $digests
      | if ($digests | length) == 1 and .Result.ResultDeprecated.digest == $digests[0]
        then $digests[0]
        else error("ambiguous exported image digest")
        end
    ' <<< "$config_json"
  )"
  if [[ ! "$result_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "ERROR: UI build record returned invalid image digest $result_digest." >&2
    return 1
  fi
  expected_builder="https://github.com/$UI_SOURCE_REPOSITORY/actions/runs/$run_id/attempts/$run_attempt"
  expected_tag="$UI_IMAGE_REPOSITORY:${source_sha:0:7}"
  if ! jq -e \
      --arg sha "$source_sha" \
      --arg source "https://github.com/$UI_SOURCE_REPOSITORY" \
      --arg builder "builder-id=$expected_builder" \
      --arg tag "$expected_tag" \
      '
        .FrontendAttrs["label:org.opencontainers.image.revision"] == $sha and
        .FrontendAttrs["label:org.opencontainers.image.source"] == $source and
        (.FrontendAttrs["attest:provenance"] | contains($builder)) and
        (
          [.Exporters[] | select(.Type == "image") | .Attrs.name | split(",")[]]
          | any(. == $tag)
        )
      ' >/dev/null <<< "$config_json"; then
    echo "ERROR: UI build record is not bound to repository, run, source commit, and expected commit tag." >&2
    return 1
  fi
  image="$UI_IMAGE_REPOSITORY@$result_digest"
  verify_image_revision "$image" "$source_sha"
  printf '%s' "$image"
)

load_run_image_digests() {
  local run_id=$1
  local source_sha=$2
  local output=$3
  local tmp_dir component artifact record record_component repository digest record_sha
  tmp_dir="$(mktemp -d)"
  : > "$output"
  if ! gh run download "$run_id" \
      --repo "$SOURCE_REPOSITORY" \
      --pattern 'image-digest-*' \
      --dir "$tmp_dir"; then
    echo "ERROR: could not download immutable image digest artifacts from workflow run $run_id." >&2
    rm -rf "$tmp_dir"
    return 1
  fi
  for component in \
    api-gateway \
    provision-worker \
    crucible-engine \
    synthetic-api-monitor \
    crucible-runner; do
    artifact="$tmp_dir/image-digest-$component/image-digest.tsv"
    if [ ! -f "$artifact" ]; then
      echo "ERROR: workflow run $run_id is missing image-digest-$component." >&2
      rm -rf "$tmp_dir"
      return 1
    fi
    record="$(cat "$artifact")"
    if [ "$(printf '%s\n' "$record" | wc -l | tr -d ' ')" != "1" ]; then
      echo "ERROR: workflow digest artifact for $component is not exactly one record." >&2
      rm -rf "$tmp_dir"
      return 1
    fi
    IFS=$'\t' read -r record_component repository digest record_sha <<< "$record"
    if [ "$record_component" != "$component" ] ||
       [ "$repository" != "ghcr.io/$SOURCE_OWNER/selfservice-$component" ] ||
       [ "$record_sha" != "$source_sha" ] ||
       [[ ! "$digest" =~ ^sha256:[0-9a-fA-F]{64}$ ]]; then
      echo "ERROR: workflow digest artifact for $component has invalid component, repository, digest, or source identity." >&2
      rm -rf "$tmp_dir"
      return 1
    fi
    printf '%s\t%s\t%s\n' "$component" "$repository" "$digest" >> "$output"
  done
  rm -rf "$tmp_dir"
}

resolve_commit_image() {
  local component=$1
  local source_sha=$2
  local run_digest=$3
  local package="selfservice-$component"
  local rows matches digest
  rows="$(
    gh api --paginate \
      "/users/$SOURCE_OWNER/packages/container/$package/versions?per_page=100" \
      --jq '.[] | [.name, ((.metadata.container.tags // []) | join(","))] | @tsv'
  )"
  matches="$(
    awk -F '\t' -v tag="$source_sha" '
      {
        count = split($2, tags, ",")
        selected = 0
        for (i = 1; i <= count; i++) {
          if (tags[i] == tag) {
            selected = 1
          }
        }
        if (selected) {
          for (i = 1; i <= count; i++) {
            if (tags[i] != tag && length(tags[i]) == 40 && tags[i] ~ /^[0-9a-fA-F]+$/) {
              print "ERROR: digest " $1 " also carries conflicting full commit tag " tags[i] "." > "/dev/stderr"
              exit 42
            }
          }
          print $1
        }
      }
    ' <<< "$rows" | sort -u
  )"
  if [ "$(printf '%s\n' "$matches" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' ')" != "1" ]; then
    echo "ERROR: GHCR package $package does not have exactly one digest tagged with full source commit $source_sha." >&2
    return 1
  fi
  digest=$matches
  if [[ ! "$digest" =~ ^sha256:[0-9a-fA-F]{64}$ ]]; then
    echo "ERROR: GHCR package $package returned invalid digest $digest for commit $source_sha." >&2
    return 1
  fi
  if [ "$digest" != "$run_digest" ]; then
    echo "ERROR: GHCR digest $digest for $component differs from workflow artifact digest $run_digest." >&2
    return 1
  fi
  local image="ghcr.io/$SOURCE_OWNER/$package@$digest"
  verify_image_revision "$image" "$source_sha" || return 1
  printf '%s' "$image"
}

resolve_candidate_images() {
  local source_sha=$1
  local output=$2
  local component image run_digests run_digest
  run_digests="$output.run-digests"
  : > "$output"
  load_run_image_digests "$PROVEN_WORKFLOW_RUN_ID" "$source_sha" "$run_digests"
  for component in \
    api-gateway \
    provision-worker \
    crucible-engine \
    synthetic-api-monitor \
    crucible-runner; do
    run_digest="$(
      awk -F '\t' -v component="$component" '
        $1 == component { print $3; matches++ }
        END { if (matches != 1) exit 3 }
      ' "$run_digests"
    )"
    image="$(resolve_commit_image "$component" "$source_sha" "$run_digest")"
    printf '%s\t%s\t%s\n' \
      "$component" \
      "ghcr.io/$SOURCE_OWNER/selfservice-$component" \
      "$image" >> "$output"
  done
  rm -f "$run_digests"
}

selector_from_manifest() {
  awk '
    function trim_yaml_value(value) {
      sub(/^[[:space:]]*/, "", value)
      sub(/[[:space:]]*$/, "", value)
      gsub(/^["'"'"']|["'"'"']$/, "", value)
      return value
    }

    /^spec:[[:space:]]*$/ {
      top_spec = 1
      next
    }
    top_spec && /^  selector:[[:space:]]*$/ {
      selector = 1
      next
    }
    selector && /^    matchLabels:[[:space:]]*$/ {
      labels = 1
      next
    }
    labels && /^      [^[:space:]][^:]*:[[:space:]]*/ {
      line = $0
      sub(/^      /, "", line)
      key = line
      sub(/:.*/, "", key)
      value = line
      sub(/^[^:]*:[[:space:]]*/, "", value)
      value = trim_yaml_value(value)
      if (result != "") {
        result = result ","
      }
      result = result key "=" value
      next
    }
    labels && !/^      / {
      labels = 0
    }
    END {
      if (result == "") {
        exit 3
      }
      print result
    }
  '
}

latest_cronjob_job() {
  local name=$1
  kubectl get jobs -n "$NAMESPACE" --sort-by=.metadata.creationTimestamp \
    -o go-template="{{range .items}}{{\$job := .}}{{range .metadata.ownerReferences}}{{if and (eq .kind \"CronJob\") (eq .name \"$name\")}}{{\$job.metadata.name}}{{\"\\n\"}}{{end}}{{end}}{{end}}" \
    | tail -n 1
}

live_effective_image() {
  local kind=$1
  local name=$2
  local container_type=$3
  local container_name=$4
  local declared_image=$5
  local live_manifest=$6
  local selector status_path image_ids image_id repository

  case "$kind" in
    CronJob)
      local job_name
      job_name="$(latest_cronjob_job "$name")"
      if [ -z "$job_name" ]; then
        echo "ERROR: missing image inventory: CronJob/$name has no retained Job to prove its effective image." >&2
        return 1
      fi
      selector="job-name=$job_name"
      ;;
    Job)
      selector="job-name=$name"
      ;;
    Deployment|DaemonSet|StatefulSet)
      if ! selector="$(selector_from_manifest < "$live_manifest")"; then
        echo "ERROR: cannot determine a pod selector for $kind/$name." >&2
        return 1
      fi
      ;;
    *)
      echo "ERROR: unsupported workload kind $kind." >&2
      return 1
      ;;
  esac

  if [ "$container_type" = initContainers ]; then
    status_path=initContainerStatuses
  else
    status_path=containerStatuses
  fi
  image_ids="$(
    kubectl get pods -n "$NAMESPACE" -l "$selector" \
      -o "jsonpath={range .items[*].status.${status_path}[?(@.name==\"${container_name}\")]}{.imageID}{\"\\n\"}{end}" \
      | sed \
          -e 's#^docker-pullable://##' \
          -e 's#^docker://##' \
          -e 's#^containerd://##' \
          -e '/^[[:space:]]*$/d' \
      | sort -u
  )"
  if [ "$(printf '%s\n' "$image_ids" | sed -e '/^[[:space:]]*$/d' | wc -l | tr -d ' ')" != "1" ]; then
    echo "ERROR: missing image inventory: $kind/$name $container_type/$container_name does not report one stable live ImageID." >&2
    return 1
  fi
  image_id=$image_ids
  if is_digest_image "$image_id"; then
    printf '%s' "$image_id"
    return 0
  fi
  if [[ "$image_id" =~ ^sha256:[0-9a-fA-F]{64}$ ]]; then
    repository="$(canonical_image_repository "$declared_image")"
    printf '%s@%s' "$repository" "$image_id"
    return 0
  fi
  echo "ERROR: live $kind/$name $container_type/$container_name ImageID is not an immutable sha256 identity: $image_id" >&2
  return 1
}

latest_helm_revision_record() {
  helm history "$RELEASE" -n "$NAMESPACE" --max 1 -o yaml \
    | awk '
        $1 == "revision:" {
          revision = $2
          gsub(/^["'"'"']|["'"'"']$/, "", revision)
        }
        $1 == "status:" {
          status = $2
          gsub(/^["'"'"']|["'"'"']$/, "", status)
        }
        END {
          if (revision == "" || status == "") {
            exit 3
          }
          print revision, status
        }
      '
}

latest_deployed_helm_revision() {
  helm history "$RELEASE" -n "$NAMESPACE" --max 256 -o yaml \
    | awk '
        function retain_deployed() {
          if (revision != "" && status == "deployed") {
            latest = revision
          }
        }
        $1 == "-" {
          retain_deployed()
          revision = ""
          status = ""
        }
        $1 == "revision:" {
          revision = $2
          gsub(/^["'"'"']|["'"'"']$/, "", revision)
        }
        $1 == "status:" {
          status = $2
          gsub(/^["'"'"']|["'"'"']$/, "", status)
        }
        END {
          retain_deployed()
          if (latest == "") {
            exit 3
          }
          print latest
        }
      '
}

verify_rollback_manifest_safety() {
  local revision status manifest tmp_dir inventory image claims worker_replicas runner
  if ! read -r revision status <<< "$(latest_helm_revision_record)"; then
    echo "ERROR: could not determine the latest Helm revision and status." >&2
    return 1
  fi
  if [ "$status" != "deployed" ]; then
    echo "ERROR: latest Helm revision $revision status is $status, not deployed; it cannot be a rollback baseline." >&2
    return 1
  fi
  if [ "$revision" != "$REQUIRED_ROLLBACK_REVISION" ]; then
    echo "ERROR: latest deployed Helm revision is $revision, not required immutable rollback revision $REQUIRED_ROLLBACK_REVISION." >&2
    return 1
  fi
  if ! manifest="$(helm get manifest "$RELEASE" -n "$NAMESPACE" --revision "$revision")"; then
    echo "ERROR: could not read manifest for deployed Helm revision $revision." >&2
    return 1
  fi
  tmp_dir="$(mktemp -d)"
  printf '%s\n' "$manifest" > "$tmp_dir/manifest.yaml"
  inventory="$tmp_dir/inventory.tsv"
  if ! manifest_workload_inventory "$tmp_dir/manifest.yaml" > "$inventory"; then
    rm -rf "$tmp_dir"
    return 1
  fi
  require_core_workloads "$inventory" || { rm -rf "$tmp_dir"; return 1; }
  while IFS=$'\t' read -r _ _ _ _ image; do
    if ! is_digest_image "$image"; then
      echo "ERROR: current Helm rollback target contains a mutable or non-sha256 image: $image" >&2
      rm -rf "$tmp_dir"
      return 1
    fi
  done < "$inventory"
  claims="$(claims_from_manifest < "$tmp_dir/manifest.yaml")"
  worker_replicas="$(rendered_worker_replicas_from_manifest "$tmp_dir/manifest.yaml")"
  runner="$(runner_image_from_manifest < "$tmp_dir/manifest.yaml")"
  if [ "$claims" != "false" ]; then
    echo "ERROR: current Helm rollback target renders worker provisioning claims as $claims, not false." >&2
    rm -rf "$tmp_dir"
    return 1
  fi
  if [ "$worker_replicas" != "1" ]; then
    echo "ERROR: current Helm rollback target renders $worker_replicas workers, not exactly 1." >&2
    rm -rf "$tmp_dir"
    return 1
  fi
  if ! is_digest_image "$runner"; then
    echo "ERROR: current Helm rollback target leaves RUNNER_IMAGE mutable: $runner" >&2
    rm -rf "$tmp_dir"
    return 1
  fi
  if ! validate_foundation_intent "$tmp_dir/manifest.yaml" true false; then
    echo "ERROR: current Helm rollback target changes the claims/content-filter foundation intent." >&2
    rm -rf "$tmp_dir"
    return 1
  fi
  STATIC_BASELINE_IMAGE_INVENTORY_SHA256="$(
    LC_ALL=C sort "$inventory" | sha256sum | awk '{print $1}'
  )"
  STATIC_BASELINE_RUNNER_IMAGE=$runner
  rm -rf "$tmp_dir"
  STATIC_BASELINE_REVISION=$revision
  echo "==> stored rollback revision $revision is claims-disabled and pins every workload image plus RUNNER_IMAGE"
}

pause_live_provisioning_claims() {
  local live_worker live_claims
  live_worker="$(mktemp)"
  kubectl get "Deployment/$RELEASE-worker" -n "$NAMESPACE" -o yaml > "$live_worker"
  live_claims="$(claims_from_manifest < "$live_worker")"
  rm -f "$live_worker"
  case "$live_claims" in
    false)
      echo "==> live worker provisioning claims are already paused"
      ;;
    true)
      echo "==> pausing live worker provisioning claims before the guarded apply"
      kubectl set env "deployment/$RELEASE-worker" \
        -n "$NAMESPACE" \
        WORKER_PROVISIONING_CLAIMS_ENABLED=false
      kubectl rollout status "deployment/$RELEASE-worker" -n "$NAMESPACE" --timeout=5m
      ;;
    *)
      echo "ERROR: live worker provisioning claims have invalid value $live_claims." >&2
      return 1
      ;;
  esac
}

verify_rollback_containment() {
  local expected_migration=${1:-}
  local required_revision=${2:-}
  local revision status manifest inventory
  if ! read -r revision status <<< "$(latest_helm_revision_record)"; then
    echo "ERROR: could not determine the latest Helm revision and status." >&2
    return 1
  fi
  if [ "$status" != "deployed" ]; then
    echo "ERROR: latest Helm revision $revision status is $status, not deployed; it cannot be a rollback baseline." >&2
    return 1
  fi
  if [ -n "$required_revision" ] && [ "$revision" != "$required_revision" ]; then
    echo "ERROR: latest deployed Helm revision is $revision, not required immutable rollback revision $required_revision." >&2
    return 1
  fi
  if ! manifest="$(helm get manifest "$RELEASE" -n "$NAMESPACE" --revision "$revision")"; then
    echo "ERROR: could not read manifest for deployed Helm revision $revision." >&2
    return 1
  fi
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  printf '%s\n' "$manifest" > "$tmp_dir/manifest.yaml"
  if ! verify_manifest_and_live "$tmp_dir/manifest.yaml" "$expected_migration"; then
    rm -rf "$tmp_dir"
    return 1
  fi
  inventory="$tmp_dir/inventory.tsv"
  if ! manifest_workload_inventory "$tmp_dir/manifest.yaml" > "$inventory"; then
    rm -rf "$tmp_dir"
    return 1
  fi
  VERIFIED_BASELINE_IMAGE_INVENTORY_SHA256="$(
    LC_ALL=C sort "$inventory" | sha256sum | awk '{print $1}'
  )"
  VERIFIED_BASELINE_RUNNER_IMAGE="$(runner_image_from_manifest < "$tmp_dir/manifest.yaml")"
  rm -rf "$tmp_dir"
  VERIFIED_BASELINE_REVISION=$revision
  echo "==> rollback containment verified: deployed revision $revision pins every rendered workload image, keeps claims false with one worker, and matches healthy live workloads"
}

required_migration_version() {
  find "$SCRIPT_DIR/../../internal/database/migrations" -maxdepth 1 -type f -name '*.up.sql' -print \
    | sed -E 's#^.*/([0-9]+)_.*#\1#' \
    | sort -n \
    | tail -n 1 \
    | sed -E 's/^0+//'
}

current_migration_state() {
  local postgres_pod state
  postgres_pod="$(
    kubectl get pods -n "$NAMESPACE" \
      -l "app.kubernetes.io/name=postgresql,app.kubernetes.io/instance=$RELEASE" \
      -o jsonpath='{.items[0].metadata.name}'
  )"
  if [ -z "$postgres_pod" ]; then
    echo "ERROR: cannot locate the release PostgreSQL pod to verify migration state." >&2
    return 1
  fi
  state="$(
    kubectl exec -n "$NAMESPACE" "$postgres_pod" -c postgresql -- sh -ec '
      password_file=${POSTGRES_PASSWORD_FILE:-}
      if [ -n "$password_file" ]; then
        export PGPASSWORD="$(cat "$password_file")"
      fi
      exec psql \
        -v ON_ERROR_STOP=1 \
        -U "${POSTGRES_USER:-postgres}" \
        -d "${POSTGRES_DATABASE:-${POSTGRES_DB:-postgres}}" \
        -Atc "SELECT version::text || '"'"':'"'"' || dirty::text FROM schema_migrations LIMIT 1"
    '
  )"
  case "$state" in
    *:false|*:f) printf '%s:false' "${state%%:*}" ;;
    *:true|*:t) printf '%s:true' "${state%%:*}" ;;
    *)
      echo "ERROR: invalid migration state returned by PostgreSQL: $state" >&2
      return 1
      ;;
  esac
}

require_no_active_jobs() {
  local postgres_pod active_jobs active_kubernetes_jobs
  postgres_pod="$(
    kubectl get pods -n "$NAMESPACE" \
      -l "app.kubernetes.io/name=postgresql,app.kubernetes.io/instance=$RELEASE" \
      -o jsonpath='{.items[0].metadata.name}'
  )"
  if [ -z "$postgres_pod" ]; then
    echo "ERROR: cannot locate the release PostgreSQL pod to verify durable job drain." >&2
    return 1
  fi
  active_jobs="$(
    kubectl exec -n "$NAMESPACE" "$postgres_pod" -c postgresql -- sh -ec '
      password_file=${POSTGRES_PASSWORD_FILE:-}
      if [ -n "$password_file" ]; then
        export PGPASSWORD="$(cat "$password_file")"
      fi
      exec psql \
        -v ON_ERROR_STOP=1 \
        -U "${POSTGRES_USER:-postgres}" \
        -d "${POSTGRES_DATABASE:-${POSTGRES_DB:-postgres}}" \
        -Atc "SELECT count(*) FROM jobs WHERE status IN ('"'"'claimed'"'"', '"'"'in_progress'"'"', '"'"'rollback'"'"')"
    '
  )"
  if [[ ! "$active_jobs" =~ ^[0-9]+$ ]]; then
    echo "ERROR: invalid active durable job count returned by PostgreSQL: $active_jobs" >&2
    return 1
  fi
  if [ "$active_jobs" != "0" ]; then
    echo "ERROR: $active_jobs durable jobs remain claimed, in_progress, or rollback after claims pause." >&2
    return 1
  fi
  active_kubernetes_jobs="$(
    kubectl get jobs -n "$NAMESPACE" -o json \
      | jq -er '[.items[] | select((.status.active // 0) > 0)] | length'
  )"
  if [[ ! "$active_kubernetes_jobs" =~ ^[0-9]+$ ]]; then
    echo "ERROR: invalid active Kubernetes Job count: $active_kubernetes_jobs" >&2
    return 1
  fi
  if [ "$active_kubernetes_jobs" != "0" ]; then
    echo "ERROR: $active_kubernetes_jobs Kubernetes Jobs remain active in namespace $NAMESPACE." >&2
    return 1
  fi
  if ! require_no_nonterminal_synthetic_pod_jobs "$postgres_pod"; then
    return 1
  fi
  echo "==> job drain verified: no durable claimed/in_progress/rollback work and no active Kubernetes Jobs"
}

require_no_nonterminal_synthetic_pod_jobs() {
  local postgres_pod=$1
  local synthetic_jobs
  synthetic_jobs="$(
    kubectl exec -n "$NAMESPACE" "$postgres_pod" -c postgresql -- sh -ec '
      password_file=${POSTGRES_PASSWORD_FILE:-}
      if [ -n "$password_file" ]; then
        export PGPASSWORD="$(cat "$password_file")"
      fi
      exec psql \
        -v ON_ERROR_STOP=1 \
        -U "${POSTGRES_USER:-postgres}" \
        -d "${POSTGRES_DATABASE:-${POSTGRES_DB:-postgres}}" \
        -Atc "SELECT count(*)
              FROM jobs AS j
              LEFT JOIN pods AS p
                ON p.id::text = j.payload->>'"'"'pod_id'"'"'
              WHERE j.type IN ('"'"'pod_create'"'"', '"'"'pod_destroy'"'"')
                AND j.status NOT IN ('"'"'completed'"'"', '"'"'failed'"'"')
                AND (
                  j.payload->>'"'"'pod_name'"'"' LIKE '"'"'synthetic-noop-%'"'"'
                  OR p.name LIKE '"'"'synthetic-noop-%'"'"'
                )"
    '
  )"
  if [[ ! "$synthetic_jobs" =~ ^[0-9]+$ ]]; then
    echo "ERROR: invalid nonterminal storage-mutating synthetic pod-job count: $synthetic_jobs" >&2
    return 1
  fi
  if [ "$synthetic_jobs" != "0" ]; then
    echo "ERROR: $synthetic_jobs nonterminal storage-mutating synthetic pod jobs remain, including pending work." >&2
    return 1
  fi
}

require_clean_migration() {
  local expected=${1:-}
  local state
  state="$(current_migration_state)"
  if [[ "$state" == *:true ]]; then
    echo "ERROR: database migration state is dirty: $state" >&2
    return 1
  fi
  if [ -n "$expected" ] && [ "$state" != "$expected" ]; then
    echo "ERROR: database migration changed from $expected to $state during baseline preparation." >&2
    return 1
  fi
  if [ -z "$expected" ]; then
    local required
    required="$(required_migration_version)"
    if [ -z "$required" ] || [ "$state" != "$required:false" ]; then
      echo "ERROR: database migration is $state; this checkout requires $required:false before phase-1." >&2
      return 1
    fi
  fi
  VERIFIED_MIGRATION_STATE=$state
}

require_core_workloads() {
  local inventory=$1
  local required
  for required in \
    "Deployment/$RELEASE-api" \
    "Deployment/$RELEASE-worker" \
    "Deployment/$RELEASE-engine" \
    "Deployment/$RELEASE-ui" \
    "CronJob/$RELEASE-synthetic-api-monitor" \
    "DaemonSet/$RELEASE-runner-image-warmer"; do
    if ! awk -F '\t' -v required="$required" '
        ($1 "/" $2) == required { found = 1 }
        END { exit found ? 0 : 1 }
      ' "$inventory"; then
      echo "ERROR: missing image inventory for required chart workload $required." >&2
      return 1
    fi
  done
}

workload_health() {
  local inventory=$1
  local workloads kind name state succeeded failed active job_name
  workloads="$(cut -f1,2 "$inventory" | sort -u)"
  while IFS=$'\t' read -r kind name; do
    case "$kind" in
      Deployment|DaemonSet|StatefulSet)
        kubectl rollout status "$kind/$name" -n "$NAMESPACE" --timeout=5m
        ;;
      CronJob)
        job_name="$(latest_cronjob_job "$name")"
        if [ -z "$job_name" ]; then
          echo "ERROR: CronJob/$name has no retained Job health evidence." >&2
          return 1
        fi
        state="$(kubectl get "job/$job_name" -n "$NAMESPACE" -o jsonpath='{.status.succeeded}{" "}{.status.failed}{" "}{.status.active}')"
        read -r succeeded failed active <<< "$state"
        succeeded=${succeeded:-0}
        failed=${failed:-0}
        active=${active:-0}
        if [ "$succeeded" -lt 1 ] || [ "$failed" -ne 0 ] || [ "$active" -ne 0 ]; then
          echo "ERROR: latest CronJob/$name Job $job_name is not healthy (succeeded=$succeeded failed=$failed active=$active)." >&2
          return 1
        fi
        ;;
      Job)
        state="$(kubectl get "job/$name" -n "$NAMESPACE" -o jsonpath='{.status.succeeded}{" "}{.status.failed}{" "}{.status.active}')"
        read -r succeeded failed active <<< "$state"
        succeeded=${succeeded:-0}
        failed=${failed:-0}
        active=${active:-0}
        if [ "$succeeded" -lt 1 ] || [ "$failed" -ne 0 ] || [ "$active" -ne 0 ]; then
          echo "ERROR: Job/$name is not healthy (succeeded=$succeeded failed=$failed active=$active)." >&2
          return 1
        fi
        ;;
    esac
  done <<< "$workloads"
}

build_live_image_maps() {
  local candidate_manifest=$1
  local candidate_inventory=$2
  local desired_map=$3
  local declared_map=$4
  local live_dir=$5
  local workloads kind name live_manifest live_inventory candidate_keys live_keys
  local candidate_type candidate_container candidate_image live_row live_image effective_image

  : > "$desired_map"
  : > "$declared_map"
  mkdir -p "$live_dir"
  workloads="$(cut -f1,2 "$candidate_inventory" | sort -u)"
  while IFS=$'\t' read -r kind name; do
    live_manifest="$live_dir/${kind}-${name}.yaml"
    if ! kubectl get "$kind/$name" -n "$NAMESPACE" -o yaml > "$live_manifest"; then
      echo "ERROR: rendered workload $kind/$name is missing from the live release." >&2
      return 1
    fi
    live_inventory="$live_dir/${kind}-${name}.inventory"
    if ! manifest_workload_inventory "$live_manifest" > "$live_inventory"; then
      return 1
    fi
    candidate_keys="$live_dir/${kind}-${name}.candidate-keys"
    live_keys="$live_dir/${kind}-${name}.live-keys"
    awk -F '\t' -v kind="$kind" -v name="$name" '
      $1 == kind && $2 == name { print $1 "\t" $2 "\t" $3 "\t" $4 }
    ' "$candidate_inventory" | sort > "$candidate_keys"
    cut -f1-4 "$live_inventory" | sort > "$live_keys"
    if ! diff -u "$candidate_keys" "$live_keys" > "$live_dir/${kind}-${name}.inventory.diff"; then
      echo "ERROR: live $kind/$name container inventory differs from the baseline chart." >&2
      head -200 "$live_dir/${kind}-${name}.inventory.diff" >&2
      return 1
    fi

    while IFS=$'\t' read -r _ _ candidate_type candidate_container candidate_image; do
      live_row="$(
        awk -F '\t' \
          -v kind="$kind" \
          -v name="$name" \
          -v type="$candidate_type" \
          -v container="$candidate_container" \
          '$1 == kind && $2 == name && $3 == type && $4 == container { print; matches++ }
           END { if (matches != 1) exit 3 }' \
          "$live_inventory"
      )"
      live_image="$(printf '%s\n' "$live_row" | cut -f5)"
      if [ "$(canonical_image_repository "$candidate_image")" != "$(canonical_image_repository "$live_image")" ]; then
        echo "ERROR: baseline chart changes $kind/$name $candidate_type/$candidate_container image repository from $live_image to $candidate_image." >&2
        return 1
      fi
      effective_image="$(
        live_effective_image \
          "$kind" \
          "$name" \
          "$candidate_type" \
          "$candidate_container" \
          "$live_image" \
          "$live_manifest"
      )"
      printf '%s\t%s\t%s\t%s\t%s\n' \
        "$kind" "$name" "$candidate_type" "$candidate_container" "$effective_image" >> "$desired_map"
      printf '%s\t%s\t%s\t%s\t%s\n' \
        "$kind" "$name" "$candidate_type" "$candidate_container" "$live_image" >> "$declared_map"
    done < <(
      awk -F '\t' -v kind="$kind" -v name="$name" '
        $1 == kind && $2 == name { print }
      ' "$candidate_inventory"
    )
  done <<< "$workloads"

  if [ "$(sort "$desired_map" | uniq | wc -l | tr -d ' ')" != "$(wc -l < "$candidate_inventory" | tr -d ' ')" ]; then
    echo "ERROR: missing image inventory while constructing the all-workload baseline." >&2
    return 1
  fi

  BASELINE_RUNNER_IMAGE="$(
    awk -F '\t' -v name="$RELEASE-runner-image-warmer" '
      $1 == "DaemonSet" && $2 == name && $3 == "containers" && $4 == "warmer" {
        print $5
        matches++
      }
      END { if (matches != 1) exit 3 }
    ' "$desired_map"
  )"
  LIVE_RUNNER_IMAGE="$(
    extract_workload_manifest "$live_dir/Deployment-${RELEASE}-engine.yaml" Deployment "$RELEASE-engine" \
      | runner_image_from_manifest
  )"
  local candidate_runner
  candidate_runner="$(
    extract_workload_manifest "$candidate_manifest" Deployment "$RELEASE-engine" \
      | runner_image_from_manifest
  )"
  if [ "$(canonical_image_repository "$candidate_runner")" != "$(canonical_image_repository "$BASELINE_RUNNER_IMAGE")" ] ||
     [ "$(canonical_image_repository "$LIVE_RUNNER_IMAGE")" != "$(canonical_image_repository "$BASELINE_RUNNER_IMAGE")" ]; then
    echo "ERROR: engine RUNNER_IMAGE and the live runner-image-warmer do not identify the same runner repository." >&2
    return 1
  fi
}

build_candidate_image_maps() {
  local candidate_manifest=$1
  local candidate_inventory=$2
  local resolved_images=$3
  local desired_map=$4
  local external_map=$5
  local live_dir=$6
  local ui_candidate_image=$7
  local workloads kind name live_manifest live_inventory candidate_keys live_keys
  local candidate_type candidate_container candidate_image candidate_repository
  local resolved_row resolved_component resolved_image live_row live_image effective_image
  local used_components="$live_dir/used-components"
  local ui_candidates=0

  mkdir -p "$live_dir"
  : > "$desired_map"
  : > "$external_map"
  : > "$used_components"
  workloads="$(cut -f1,2 "$candidate_inventory" | sort -u)"
  while IFS=$'\t' read -r kind name; do
    live_manifest="$live_dir/${kind}-${name}.yaml"
    if ! kubectl get "$kind/$name" -n "$NAMESPACE" -o yaml > "$live_manifest"; then
      echo "ERROR: rendered candidate workload $kind/$name is missing from the proven live release." >&2
      return 1
    fi
    live_inventory="$live_dir/${kind}-${name}.inventory"
    manifest_workload_inventory "$live_manifest" > "$live_inventory"
    candidate_keys="$live_dir/${kind}-${name}.candidate-keys"
    live_keys="$live_dir/${kind}-${name}.live-keys"
    awk -F '\t' -v kind="$kind" -v name="$name" '
      $1 == kind && $2 == name { print $1 "\t" $2 "\t" $3 "\t" $4 }
    ' "$candidate_inventory" | sort > "$candidate_keys"
    cut -f1-4 "$live_inventory" | sort > "$live_keys"
    if ! diff -u "$candidate_keys" "$live_keys" > "$live_dir/${kind}-${name}.inventory.diff"; then
      echo "ERROR: candidate $kind/$name container inventory differs from the proven live release." >&2
      head -200 "$live_dir/${kind}-${name}.inventory.diff" >&2
      return 1
    fi

    while IFS=$'\t' read -r _ _ candidate_type candidate_container candidate_image; do
      candidate_repository="$(canonical_image_repository "$candidate_image")"
      resolved_row="$(
        awk -F '\t' -v repository="$candidate_repository" '
          $2 == repository { print; matches++ }
          END { if (matches > 1) exit 3 }
        ' "$resolved_images"
      )"
      if [ -n "$resolved_row" ]; then
        resolved_component="$(printf '%s\n' "$resolved_row" | cut -f1)"
        resolved_image="$(printf '%s\n' "$resolved_row" | cut -f3)"
        printf '%s\n' "$resolved_component" >> "$used_components"
        printf '%s\t%s\t%s\t%s\t%s\n' \
          "$kind" "$name" "$candidate_type" "$candidate_container" "$resolved_image" >> "$desired_map"
        continue
      fi

      live_row="$(
        awk -F '\t' \
          -v kind="$kind" \
          -v name="$name" \
          -v type="$candidate_type" \
          -v container="$candidate_container" \
          '$1 == kind && $2 == name && $3 == type && $4 == container { print; matches++ }
           END { if (matches != 1) exit 3 }' \
          "$live_inventory"
      )"
      live_image="$(printf '%s\n' "$live_row" | cut -f5)"
      if [ "$candidate_repository" != "$(canonical_image_repository "$live_image")" ]; then
        echo "ERROR: candidate changes external $kind/$name $candidate_type/$candidate_container repository from $live_image to $candidate_image." >&2
        return 1
      fi
      if [ "$candidate_repository" = "$(canonical_image_repository "$UI_IMAGE_REPOSITORY")" ]; then
        if [ "$kind/$name/$candidate_type/$candidate_container" != "Deployment/$RELEASE-ui/containers/ui" ]; then
          echo "ERROR: UI candidate repository appears in unexpected workload container $kind/$name $candidate_type/$candidate_container." >&2
          return 1
        fi
        printf '%s\t%s\t%s\t%s\t%s\n' \
          "$kind" "$name" "$candidate_type" "$candidate_container" "$ui_candidate_image" >> "$desired_map"
        ui_candidates=$((ui_candidates + 1))
        continue
      fi
      effective_image="$(
        live_effective_image \
          "$kind" \
          "$name" \
          "$candidate_type" \
          "$candidate_container" \
          "$live_image" \
          "$live_manifest"
      )"
      printf '%s\t%s\t%s\t%s\t%s\n' \
        "$kind" "$name" "$candidate_type" "$candidate_container" "$effective_image" \
        | tee -a "$desired_map" >> "$external_map"
    done < <(
      awk -F '\t' -v kind="$kind" -v name="$name" '
        $1 == kind && $2 == name { print }
      ' "$candidate_inventory"
    )
  done <<< "$workloads"

  if [ "$ui_candidates" -ne 1 ]; then
    echo "ERROR: final candidate must replace exactly one selfservice-ui container with the proven UI image." >&2
    return 1
  fi
  local component
  for component in \
    api-gateway \
    provision-worker \
    crucible-engine \
    synthetic-api-monitor \
    crucible-runner; do
    if ! grep -qx "$component" "$used_components"; then
      echo "ERROR: final candidate does not use the commit-bound $component image." >&2
      return 1
    fi
  done
  if [ "$(sort "$desired_map" | uniq | wc -l | tr -d ' ')" != "$(wc -l < "$candidate_inventory" | tr -d ' ')" ]; then
    echo "ERROR: missing image inventory while constructing the production candidate." >&2
    return 1
  fi

  CANDIDATE_RUNNER_IMAGE="$(
    awk -F '\t' '
      $1 == "crucible-runner" { print $3; matches++ }
      END { if (matches != 1) exit 3 }
    ' "$resolved_images"
  )"
  local candidate_runner
  candidate_runner="$(
    extract_workload_manifest "$candidate_manifest" Deployment "$RELEASE-engine" \
      | runner_image_from_manifest
  )"
  if [ "$(canonical_image_repository "$candidate_runner")" != "$(canonical_image_repository "$CANDIDATE_RUNNER_IMAGE")" ]; then
    echo "ERROR: candidate engine RUNNER_IMAGE does not use the commit-bound crucible-runner repository." >&2
    return 1
  fi
}

extract_upgrade_hooks() {
  local manifest=$1
  local output=$2
  awk '
    function flush() {
      if (upgrade_hook) {
        if (written) {
          print "---"
        }
        printf "%s", document
        written = 1
      }
      document = ""
      upgrade_hook = 0
    }
    /^---[[:space:]]*$/ {
      flush()
      next
    }
    {
      document = document $0 ORS
      if ($0 ~ /helm\.sh\/hook:[[:space:]]*["'"'"']?([^"'"'"']*,[[:space:]]*)*(pre-upgrade|post-upgrade)([[:space:]]*,[^"'"'"']*)?["'"'"']?[[:space:]]*$/) {
        upgrade_hook = 1
      }
    }
    END {
      flush()
    }
  ' "$manifest" > "$output"
}

strip_helm_hooks() {
  local manifest=$1
  local output=$2
  awk '
    function flush() {
      if (!helm_hook && document != "") {
        print "---"
        printf "%s", document
      }
      document = ""
      helm_hook = 0
    }
    /^---[[:space:]]*$/ {
      flush()
      next
    }
    {
      document = document $0 ORS
      if ($0 ~ /helm\.sh\/hook:[[:space:]]*/) {
        helm_hook = 1
      }
    }
    END {
      flush()
    }
  ' "$manifest" > "$output"
}

build_upgrade_hook_image_map() {
  local hook_inventory=$1
  local resolved_images=$2
  local candidate_images=$3
  local output=$4
  local kind name container_type container_name image repository resolved_row resolved_image
  local matches candidate_row candidate_image
  : > "$output"
  while IFS=$'\t' read -r kind name container_type container_name image; do
    repository="$(canonical_image_repository "$image")"
    resolved_row="$(
      awk -F '\t' -v repository="$repository" '
        $2 == repository { print; matches++ }
        END { if (matches > 1) exit 3 }
      ' "$resolved_images"
    )"
    if [ -n "$resolved_row" ]; then
      resolved_image="$(printf '%s\n' "$resolved_row" | cut -f3)"
      printf '%s\t%s\t%s\t%s\t%s\n' \
        "$kind" "$name" "$container_type" "$container_name" "$resolved_image" >> "$output"
      continue
    fi

    matches="$(
      while IFS=$'\t' read -r _ _ _ _ candidate_image; do
        if [ "$(canonical_image_repository "$candidate_image")" = "$repository" ]; then
          printf '%s\n' "$candidate_image"
        fi
      done < "$candidate_images" | sort -u
    )"
    if [ "$(printf '%s\n' "$matches" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' ')" != "1" ]; then
      echo "ERROR: upgrade hook $kind/$name $container_type/$container_name does not map to one preserved external or commit-built digest." >&2
      return 1
    fi
    candidate_row=$matches
    printf '%s\t%s\t%s\t%s\t%s\n' \
      "$kind" "$name" "$container_type" "$container_name" "$candidate_row" >> "$output"
  done < "$hook_inventory"
}

# kubectl_server_apply_dry_run is the single place in this script that may
# invoke `kubectl apply --server-side --dry-run=server`. Centralizing the
# exact, fixed argv tuple here - rather than repeating it independently at
# each call site - means there is exactly one place that needs to carry
# --server-side, --dry-run=server, and --force-conflicts as their own
# complete, literal, unparameterized shell words; every caller supplies only
# the field manager, manifest path, and output format as ordinary
# arguments, so none of those fixed flags can be disguised, concatenated
# with a caller-supplied value, or otherwise hidden inside another flag's
# text the way a raw substring scan over free-form call-site text could be
# fooled (for example a decoy such as
# --field-manager=X--dry-run=server sitting next to an actual
# --dry-run=none, which would still "contain" the safe substring while
# really performing a mutating, non-dry-run apply).
#
# --force-conflicts is paired inseparably with --dry-run=server here:
# existing production objects are owned by Helm (and one containment field
# by kubectl-set), so a validation-only server-side dry-run must be allowed
# to take over field ownership to prove admission/defaulting would succeed.
# The real apply remains `helm upgrade`, never this function, so this can
# never mutate anything or change production's actual field ownership.
#
# This function must NEVER be called from any mutating code path - only
# from the three read-only validation call sites below - because dropping
# --dry-run=server here (accidentally or otherwise) would turn
# --force-conflicts into a real, mutating field-ownership takeover against
# live production objects.
#
# The invocation below deliberately uses `command kubectl`, not a bare
# `kubectl`. Bash resolves an unqualified command name to a matching shell
# function or alias - including one defined with a `{ ... }` compound
# command OR a `( ... )` subshell body - before ever doing a PATH lookup for
# the real binary. If anything else in this file (or anything sourced by
# it) ever defined `kubectl() { ... }`, `kubectl() ( ... )`, or
# `alias kubectl=...`, a bare `kubectl` call here would silently run that
# shadow instead of the real kubectl executable, letting it inject, drop,
# reorder, or rewrite this call's argv at runtime - for example inserting an
# unrelated value-consuming flag immediately before --dry-run=server so
# kubectl's own flag parser consumes the literal string "--dry-run=server"
# as that flag's *value* instead of recognizing it as --dry-run=server,
# silently leaving dry-run at its default (server-side mutation) while
# --force-conflicts is still in effect. `command` forces bash to skip shell
# function and alias lookup entirely and go straight to a PATH search for
# the real kubectl binary, so no shadow defined anywhere - regardless of
# whether it is brace- or subshell-bodied - can ever intercept this call.
# (Aliases are in any case never expanded in a non-interactive script such
# as this one unless it explicitly runs `shopt -s expand_aliases`, which it
# does not; `command` closes the function-shadow path, which is the one
# that would otherwise work regardless of that setting.)
kubectl_server_apply_dry_run() {
  local field_manager=$1
  local manifest_path=$2
  local output_format=$3
  command kubectl apply \
    --server-side \
    --dry-run=server \
    --force-conflicts \
    --field-manager="$field_manager" \
    -n "$NAMESPACE" \
    -f "$manifest_path" \
    -o "$output_format"
}

validate_upgrade_hooks() {
  local unpinned_hooks=$1
  local resolved_images=$2
  local candidate_images=$3
  local tmp_dir=$4
  local inventory="$tmp_dir/upgrade-hook-inventory.tsv"
  local image_map="$tmp_dir/upgrade-hook-images.tsv"
  local pinned="$tmp_dir/upgrade-hooks-pinned.yaml"
  local pinned_inventory="$tmp_dir/upgrade-hooks-pinned-inventory.tsv"
  local server="$tmp_dir/upgrade-hooks-server.yaml"
  local server_inventory="$tmp_dir/upgrade-hooks-server-inventory.tsv"
  mkdir -p "$tmp_dir"
  if [ ! -s "$unpinned_hooks" ]; then
    echo "==> no pre-upgrade or post-upgrade Helm hooks are rendered"
    return 0
  fi

  manifest_workload_inventory "$unpinned_hooks" true > "$inventory"
  build_upgrade_hook_image_map "$inventory" "$resolved_images" "$candidate_images" "$image_map"
  PIN_RUNNER_REQUIRED=false \
    BASELINE_IMAGE_MAP="$image_map" \
    "$PIN_BASELINE_SCRIPT" < "$unpinned_hooks" > "$pinned"
  manifest_workload_inventory "$pinned" true > "$pinned_inventory"
  if ! diff -u <(sort "$image_map") <(sort "$pinned_inventory"); then
    echo "ERROR: upgrade-hook pinning did not produce the selected immutable image inventory." >&2
    return 1
  fi
  if ! diff -u <(sort "$image_map") <(sort "$inventory"); then
    echo "ERROR: an upgrade Helm hook uses a mutable or wrong image; Helm excludes hooks from post-renderer pinning." >&2
    return 1
  fi
  while IFS=$'\t' read -r _ _ _ _ image; do
    if ! is_digest_image "$image"; then
      echo "ERROR: upgrade Helm hook contains a mutable or unpinned image: $image" >&2
      return 1
    fi
  done < "$inventory"
  echo "==> server-side dry-run validating immutable upgrade Helm hooks"
  # See kubectl_server_apply_dry_run for why --force-conflicts is paired
  # inseparably with --dry-run=server: these hook manifests are already
  # Helm-owned, so a validation-only SSA dry-run must be allowed to take
  # over field ownership to prove admission/defaulting would succeed. This
  # never affects the real hook execution (Helm applies the hooks itself
  # during `helm upgrade`), only this read-only proof.
  kubectl_server_apply_dry_run crucible-production-deploy-hooks "$unpinned_hooks" yaml > "$server"
  manifest_workload_inventory "$server" true > "$server_inventory"
  if ! diff -u <(sort "$image_map") <(sort "$server_inventory"); then
    echo "ERROR: server-defaulted upgrade hooks changed the validated image inventory." >&2
    return 1
  fi
}

validate_foundation_intent() {
  local manifest=$1
  local validate_replicas=${2:-true}
  local validate_synthetic_lifecycle=${3:-true}
  local claims content_enabled content_feed synthetic_expected synthetic_feed synthetic_lifecycle worker_replicas
  claims="$(claims_from_manifest < "$manifest")"
  content_enabled="$(env_from_manifest WORKER_CONTENT_FILTER_ENABLED < "$manifest")"
  content_feed="$(env_from_manifest WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL < "$manifest")"
  synthetic_expected="$(env_from_manifest SYNTHETIC_CONTENT_FILTER_EXPECTED < "$manifest")"
  synthetic_feed="$(env_from_manifest SYNTHETIC_CONTENT_FILTER_CATEGORY_FEED_BASE_URL < "$manifest")"
  synthetic_lifecycle="$(env_from_manifest SYNTHETIC_LIFECYCLE_ENABLED < "$manifest")"
  if [ "$claims" != "false" ]; then
    echo "ERROR: candidate renders worker provisioning claims as $claims, not false." >&2
    return 1
  fi
  if [ "$content_enabled" != "false" ] || [ -n "$content_feed" ]; then
    echo "ERROR: candidate must keep WORKER_CONTENT_FILTER_ENABLED=false and its category feed empty." >&2
    return 1
  fi
  if [ "$synthetic_expected" != "false" ] || [ -n "$synthetic_feed" ]; then
    echo "ERROR: candidate synthetic content-filter intent must remain false with an empty category feed." >&2
    return 1
  fi
  if [ "$validate_synthetic_lifecycle" = true ] && [ "$synthetic_lifecycle" != "false" ]; then
    echo "ERROR: candidate must keep SYNTHETIC_LIFECYCLE_ENABLED=false." >&2
    return 1
  fi
  if [ "$validate_replicas" = true ]; then
    worker_replicas="$(rendered_worker_replicas_from_manifest "$manifest")"
    if [ "$worker_replicas" != "1" ]; then
      echo "ERROR: candidate renders $worker_replicas workers, not exactly 1." >&2
      return 1
    fi
  fi
}

validate_synthetic_monitor_active() {
  local manifest=$1
  local suspended
  suspended="$(synthetic_monitor_suspend_from_manifest "$manifest")"
  if [ "$suspended" != "false" ]; then
    echo "ERROR: candidate must keep non-mutating CronJob/$RELEASE-synthetic-api-monitor unsuspended." >&2
    return 1
  fi
}

validate_candidate_manifest() {
  local manifest=$1
  local expected_map=$2
  local actual_inventory=$3
  local runner_image
  manifest_workload_inventory "$manifest" > "$actual_inventory"
  require_core_workloads "$actual_inventory"
  if ! diff -u <(sort "$expected_map") <(sort "$actual_inventory"); then
    echo "ERROR: candidate post-rendering did not pin exactly the resolved workload images." >&2
    return 1
  fi
  while IFS=$'\t' read -r _ _ _ _ image; do
    if ! is_digest_image "$image"; then
      echo "ERROR: final candidate contains a mutable or unpinned workload image: $image" >&2
      return 1
    fi
  done < "$actual_inventory"
  runner_image="$(runner_image_from_manifest < "$manifest")"
  if [ "$runner_image" != "$CANDIDATE_RUNNER_IMAGE" ] || ! is_digest_image "$runner_image"; then
    echo "ERROR: final candidate RUNNER_IMAGE is not the commit-bound runner digest." >&2
    return 1
  fi
  validate_foundation_intent "$manifest"
  validate_synthetic_monitor_active "$manifest"
}

server_validate_candidate() {
  local manifest=$1
  local expected_map=$2
  local output=$3
  local canonical_output=$4
  local document_dir="$output.documents"
  local document
  echo "==> server-side dry-run validating the exact digest-pinned candidate"
  # `kubectl apply -f <multi-document-manifest> -o yaml` does not return
  # `---`-separated top-level documents the way the input was written: it
  # collapses every applied object into one `apiVersion: v1, kind: List`
  # wrapper with the individual objects nested under `items:`. This script's
  # manifest_workload_inventory only ever recognizes a document's own
  # top-level (unindented) `kind:` line, so it cannot see a Deployment,
  # DaemonSet, CronJob, etc. nested inside a List's `items:` - it silently
  # produces an empty inventory, and require_core_workloads then fails with
  # "missing image inventory for required chart workload ...". This is
  # reproduced independently against live k3s, not merely inferred.
  #
  # The fix is to never hand kubectl a multi-document `-f` input for `-o
  # yaml` here: split the exact rendered candidate into its individual
  # documents (the same split_manifest_documents used by
  # canonicalize_server_candidate below), run the dry-run apply one document
  # at a time so kubectl always returns one plain top-level object per call,
  # and reassemble those server-defaulted objects into a `---`-separated
  # multi-document manifest ourselves before handing it to
  # validate_candidate_manifest. Running the per-document dry-run again here
  # (rather than only in canonicalize_server_candidate) duplicates work, but
  # keeps this function's inventory/digest validation and the canonical
  # comparison independent, exact-equality checks.
  rm -rf "$document_dir"
  if ! split_manifest_documents "$manifest" "$document_dir"; then
    echo "ERROR: candidate manifest has no YAML documents to server-side dry-run validate." >&2
    rm -rf "$document_dir"
    return 1
  fi
  : > "$output"
  for document in "$document_dir"/*.yaml; do
    # See kubectl_server_apply_dry_run for why --force-conflicts is paired
    # inseparably with --dry-run=server, for the same reason as
    # canonicalize_server_candidate below: this is a per-document
    # validation-only SSA dry-run against already Helm-owned objects, not a
    # mutating apply. This script runs under `set -euo pipefail` and this
    # call is not guarded by `if`/`&&`/`||`, so a failure on any one
    # document aborts the whole deploy immediately - this function's caller
    # can never reach a partial $output, validate_candidate_manifest, or
    # Helm with an incomplete candidate.
    kubectl_server_apply_dry_run crucible-production-deploy "$document" yaml >> "$output"
    printf -- '---\n' >> "$output"
  done
  rm -rf "$document_dir"
  validate_candidate_manifest "$output" "$expected_map" "$output.inventory"
  canonicalize_server_candidate "$manifest" "$canonical_output"
}

split_manifest_documents() {
  local manifest=$1
  local output_dir=$2
  mkdir -p "$output_dir"
  awk -v output_dir="$output_dir" '
    function flush() {
      if (document != "") {
        file = sprintf("%s/%06d.yaml", output_dir, ++documents)
        printf "%s", document > file
        close(file)
      }
      document = ""
    }
    /^---[[:space:]]*$/ {
      flush()
      next
    }
    {
      document = document $0 ORS
    }
    END {
      flush()
      if (documents == 0) {
        exit 3
      }
    }
  ' "$manifest"
}

canonicalize_server_candidate() {
  local manifest=$1
  local output=$2
  local document_dir="$output.documents"
  local rows="$output.rows"
  local document
  rm -rf "$document_dir"
  split_manifest_documents "$manifest" "$document_dir"
  : > "$rows"
  for document in "$document_dir"/*.yaml; do
    # See kubectl_server_apply_dry_run for why --force-conflicts is paired
    # inseparably with --dry-run=server, for the same reason as
    # server_validate_candidate above: this is a per-document validation-only
    # SSA dry-run against already Helm-owned objects, not a mutating apply.
    kubectl_server_apply_dry_run crucible-production-deploy "$document" json \
      | jq -cS '
          del(
            .metadata.creationTimestamp,
            .metadata.deletionGracePeriodSeconds,
            .metadata.deletionTimestamp,
            .metadata.generation,
            .metadata.managedFields,
            .metadata.resourceVersion,
            .metadata.selfLink,
            .metadata.uid,
            .status
          )
        ' >> "$rows"
  done
  LC_ALL=C sort "$rows" > "$output"
  rm -rf "$document_dir" "$rows"
}

verify_candidate_cronjob_image() {
  local name=$1
  local container_type=$2
  local container_name=$3
  local expected=$4
  local verification_job actual
  if [ "$container_type" != containers ]; then
    echo "ERROR: candidate CronJob/$name uses unsupported $container_type/$container_name for verification." >&2
    return 1
  fi
  verification_job="${name}-deploy-verify-$(date -u +%s)-$$"
  verification_job="${verification_job:0:63}"
  if ! kubectl create job \
      "$verification_job" \
      --from="cronjob/$name" \
      -n "$NAMESPACE" >/dev/null; then
    echo "ERROR: could not create contained image-verification Job from CronJob/$name." >&2
    return 1
  fi
  if ! kubectl wait \
      --for=condition=complete \
      "job/$verification_job" \
      -n "$NAMESPACE" \
      --timeout="$TIMEOUT" >/dev/null; then
    echo "ERROR: contained image-verification Job/$verification_job did not complete successfully." >&2
    return 1
  fi
  actual="$(
    live_effective_image \
      Job \
      "$verification_job" \
      "$container_type" \
      "$container_name" \
      "$expected" \
      /dev/null
  )"
  if [ "$actual" != "$expected" ]; then
    echo "ERROR: candidate live image drifted for CronJob/$name $container_type/$container_name: expected $expected, found $actual." >&2
    return 1
  fi
}

verify_external_candidate_images() {
  local external_map=$1
  local tmp_dir=$2
  local image_class=${3:-external}
  local kind name container_type container_name expected live_manifest live_inventory live_row declared actual
  mkdir -p "$tmp_dir"
  while IFS=$'\t' read -r kind name container_type container_name expected; do
    live_manifest="$tmp_dir/${kind}-${name}.yaml"
    live_inventory="$tmp_dir/${kind}-${name}.inventory"
    if [ ! -f "$live_manifest" ]; then
      kubectl get "$kind/$name" -n "$NAMESPACE" -o yaml > "$live_manifest"
      manifest_workload_inventory "$live_manifest" > "$live_inventory"
    fi
    live_row="$(
      awk -F '\t' \
        -v kind="$kind" \
        -v name="$name" \
        -v type="$container_type" \
        -v container="$container_name" \
        '$1 == kind && $2 == name && $3 == type && $4 == container { print; matches++ }
         END { if (matches != 1) exit 3 }' \
        "$live_inventory"
    )"
    declared="$(printf '%s\n' "$live_row" | cut -f5)"
    if [ "$image_class" = candidate ]; then
      if [ "$declared" != "$expected" ]; then
        echo "ERROR: candidate declared image drifted for $kind/$name $container_type/$container_name: expected $expected, found $declared." >&2
        return 1
      fi
      if [ "$kind" = CronJob ]; then
        if ! verify_candidate_cronjob_image \
            "$name" \
            "$container_type" \
            "$container_name" \
            "$expected"; then
          return 1
        fi
        continue
      fi
    fi
    actual="$(
      live_effective_image \
        "$kind" \
        "$name" \
        "$container_type" \
        "$container_name" \
        "$declared" \
        "$live_manifest"
    )"
    if [ "$actual" != "$expected" ]; then
      echo "ERROR: $image_class live image drifted for $kind/$name $container_type/$container_name: expected $expected, found $actual." >&2
      return 1
    fi
  done < "$external_map"
}

verify_deployed_candidate() {
  local revision status tmp_dir manifest inventory deployed_canonical
  if ! read -r revision status <<< "$(latest_helm_revision_record)"; then
    echo "ERROR: could not determine the deployed candidate Helm revision." >&2
    return 1
  fi
  if [ "$status" != "deployed" ] || [ "$revision" = "$ROLLBACK_BASELINE_REVISION" ]; then
    echo "ERROR: candidate Helm revision is $revision status $status after atomic upgrade." >&2
    return 1
  fi
  tmp_dir="$CANDIDATE_TMP_DIR/deployed-candidate"
  mkdir -p "$tmp_dir"
  manifest="$tmp_dir/manifest.yaml"
  inventory="$tmp_dir/inventory.tsv"
  if ! helm get manifest "$RELEASE" -n "$NAMESPACE" --revision "$revision" > "$manifest"; then
    echo "ERROR: could not read deployed candidate revision $revision." >&2
    return 1
  fi
  if ! validate_candidate_manifest "$manifest" "$CANDIDATE_IMAGE_MAP" "$inventory"; then
    echo "ERROR: deployed Helm manifest differs from the validated immutable candidate." >&2
    return 1
  fi
  deployed_canonical="$tmp_dir/server.canonical"
  if ! canonicalize_server_candidate "$manifest" "$deployed_canonical"; then
    echo "ERROR: could not canonicalize the deployed Helm object set." >&2
    return 1
  fi
  if ! diff -u "$CANDIDATE_SERVER_CANONICAL" "$deployed_canonical"; then
    echo "ERROR: deployed Helm object set differs from the exact validated candidate." >&2
    return 1
  fi
  if ! require_no_active_jobs; then
    return 1
  fi
  if ! verify_external_candidate_images \
      "$CANDIDATE_IMAGE_MAP" \
      "$tmp_dir/live-images" \
      candidate; then
    return 1
  fi
  if ! workload_health "$inventory"; then
    echo "ERROR: deployed candidate workloads are not healthy." >&2
    return 1
  fi
  if ! require_no_active_jobs; then
    return 1
  fi
  echo "==> deployed candidate revision $revision matches every declared/live image and remains fully drained"
}

enforce_synthetic_rollback_containment() {
  local api_cronjob="$RELEASE-synthetic-api-monitor"
  local clone_cronjob suspended tmp_manifest
  if ! kubectl patch "cronjob/$api_cronjob" \
      -n "$NAMESPACE" \
      --type strategic \
      -p '{"spec":{"suspend":false,"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"synthetic-api-monitor","env":[{"name":"SYNTHETIC_LIFECYCLE_ENABLED","value":"false"}]}]}}}}}}'; then
    echo "ERROR: could not contain the API monitor lifecycle after atomic rollback." >&2
    return 1
  fi
  for clone_cronjob in "$RELEASE-synthetic-janitor" "$RELEASE-synthetic-runner"; do
    if kubectl get "cronjob/$clone_cronjob" -n "$NAMESPACE" >/dev/null 2>&1; then
      if ! kubectl patch "cronjob/$clone_cronjob" \
          -n "$NAMESPACE" \
          --type merge \
          -p '{"spec":{"suspend":true}}'; then
        echo "ERROR: could not suspend clone-producing CronJob/$clone_cronjob after atomic rollback." >&2
        return 1
      fi
    fi
  done
  tmp_manifest="$(mktemp)"
  if ! kubectl get "cronjob/$api_cronjob" -n "$NAMESPACE" -o yaml > "$tmp_manifest"; then
    rm -f "$tmp_manifest"
    echo "ERROR: API monitor rollback containment could not read back CronJob/$api_cronjob." >&2
    return 1
  fi
  if ! validate_synthetic_monitor_active "$tmp_manifest"; then
    rm -f "$tmp_manifest"
    echo "ERROR: API monitor rollback containment did not preserve the unsuspended monitor." >&2
    return 1
  fi
  if [ "$(env_from_manifest SYNTHETIC_LIFECYCLE_ENABLED < "$tmp_manifest")" != "false" ]; then
    rm -f "$tmp_manifest"
    echo "ERROR: API monitor rollback containment did not preserve SYNTHETIC_LIFECYCLE_ENABLED=false." >&2
    return 1
  fi
  rm -f "$tmp_manifest"
  for clone_cronjob in "$RELEASE-synthetic-janitor" "$RELEASE-synthetic-runner"; do
    if kubectl get "cronjob/$clone_cronjob" -n "$NAMESPACE" >/dev/null 2>&1; then
      suspended="$(
        kubectl get "cronjob/$clone_cronjob" \
          -n "$NAMESPACE" \
          -o jsonpath='{.spec.suspend}'
      )"
      if [ "$suspended" != "true" ]; then
        echo "ERROR: clone-producing CronJob/$clone_cronjob is not suspended after atomic rollback (observed: ${suspended:-<empty>})." >&2
        return 1
      fi
    fi
  done
  echo "==> synthetic rollback containment enforced: API monitor active/non-lifecycle; clone CronJobs suspended"
}

contain_failed_atomic_upgrade() {
  echo "==> atomic upgrade failed; proving rollback containment before releasing the lock" >&2
  if ! kubectl set env "deployment/$RELEASE-worker" \
      -n "$NAMESPACE" \
      WORKER_PROVISIONING_CLAIMS_ENABLED=false; then
    echo "ERROR: could not force provisioning claims disabled after atomic failure." >&2
    return 1
  fi
  if ! kubectl rollout status "deployment/$RELEASE-worker" -n "$NAMESPACE" --timeout=5m; then
    echo "ERROR: claims-disabled worker rollout did not stabilize after atomic failure." >&2
    return 1
  fi
  if ! enforce_synthetic_rollback_containment; then
    return 1
  fi
  if ! verify_rollback_containment \
      "$ROLLBACK_BASELINE_MIGRATION"; then
    echo "ERROR: atomic rollback did not restore the approved immutable baseline." >&2
    return 1
  fi
  if [ "$VERIFIED_BASELINE_IMAGE_INVENTORY_SHA256" != "$STATIC_BASELINE_IMAGE_INVENTORY_SHA256" ] ||
     [ "$VERIFIED_BASELINE_RUNNER_IMAGE" != "$STATIC_BASELINE_RUNNER_IMAGE" ]; then
    echo "ERROR: atomic rollback revision $VERIFIED_BASELINE_REVISION does not match immutable revision-$ROLLBACK_BASELINE_REVISION image identity." >&2
    return 1
  fi
  if ! require_no_active_jobs; then
    echo "ERROR: work remained active after atomic rollback." >&2
    return 1
  fi
  echo "==> atomic failure contained at revision-$ROLLBACK_BASELINE_REVISION immutable image baseline via deployed revision $VERIFIED_BASELINE_REVISION with claims disabled and all work drained" >&2
}

validate_pinned_manifest() {
  local manifest=$1
  local expected_map=$2
  local actual_inventory=$3
  local runner_image claims
  manifest_workload_inventory "$manifest" > "$actual_inventory"
  if ! diff -u <(sort "$expected_map") <(sort "$actual_inventory"); then
    echo "ERROR: the baseline post-renderer did not pin exactly the inventoried workload images." >&2
    return 1
  fi
  while IFS=$'\t' read -r _ _ _ _ image; do
    if ! is_digest_image "$image"; then
      echo "ERROR: baseline contains a mutable workload image after pinning: $image" >&2
      return 1
    fi
  done < "$actual_inventory"
  runner_image="$(runner_image_from_manifest < "$manifest")"
  if [ "$runner_image" != "$BASELINE_RUNNER_IMAGE" ] || ! is_digest_image "$runner_image"; then
    echo "ERROR: baseline engine RUNNER_IMAGE is not pinned to the effective runner digest." >&2
    return 1
  fi
  claims="$(claims_from_manifest < "$manifest")"
  if [ "$claims" != "false" ]; then
    echo "ERROR: baseline renders worker provisioning claims as $claims, not false." >&2
    return 1
  fi
  if ! validate_foundation_intent "$manifest"; then
    echo "ERROR: prepared baseline changes the claims/content-filter foundation intent." >&2
    return 1
  fi
  if ! validate_synthetic_monitor_active "$manifest"; then
    echo "ERROR: prepared baseline would suspend the non-mutating API monitor." >&2
    return 1
  fi
}

rollout_annotation_from_manifest() {
  awk '
    function trim_yaml_value(value) {
      sub(/^[[:space:]]*/, "", value)
      sub(/[[:space:]]*$/, "", value)
      gsub(/^["'"'"']|["'"'"']$/, "", value)
      return value
    }
    {
      match($0, /^[[:space:]]*/)
      indent = RLENGTH
      stripped = substr($0, indent + 1)
    }
    stripped == "template:" {
      template_indent = indent
      in_template = 1
      next
    }
    in_template && indent == template_indent + 2 && stripped == "metadata:" {
      metadata_indent = indent
      in_metadata = 1
      next
    }
    in_metadata && stripped ~ /^kubectl\.kubernetes\.io\/restartedAt:[[:space:]]*/ {
      value = stripped
      sub(/^kubectl\.kubernetes\.io\/restartedAt:[[:space:]]*/, "", value)
      value = trim_yaml_value(value)
      matches++
    }
    in_metadata && stripped != "" && indent <= metadata_indent && stripped != "metadata:" {
      in_metadata = 0
    }
    in_template && stripped != "" && indent <= template_indent && stripped != "template:" {
      in_template = 0
    }
    END {
      if (matches > 1) {
        exit 3
      }
      if (matches == 1) {
        print value
      }
    }
  '
}

set_rollout_annotation() {
  local manifest=$1
  local output=$2
  local value=$3
  local existing
  existing="$(rollout_annotation_from_manifest < "$manifest")"
  awk -v value="$value" -v replace_existing="$([ -n "$existing" ] && printf 1 || printf 0)" '
    function spaces(count,    result) {
      result = ""
      while (length(result) < count) {
        result = result " "
      }
      return result
    }
    {
      match($0, /^[[:space:]]*/)
      indent = RLENGTH
      stripped = substr($0, indent + 1)
    }
    stripped == "template:" {
      template_indent = indent
      in_template = 1
    }
    in_template && indent == template_indent + 2 && stripped == "metadata:" {
      metadata_indent = indent
      in_metadata = 1
    }
    in_metadata && stripped == "annotations:" && !inserted && !replace_existing {
      print
      print spaces(indent + 2) "kubectl.kubernetes.io/restartedAt: \"" value "\""
      inserted = 1
      next
    }
    in_metadata && stripped ~ /^kubectl\.kubernetes\.io\/restartedAt:[[:space:]]*/ && replace_existing {
      print spaces(indent) "kubectl.kubernetes.io/restartedAt: \"" value "\""
      inserted = 1
      next
    }
    in_metadata && stripped != "" && indent <= metadata_indent && stripped != "metadata:" {
      if (!inserted) {
        print spaces(metadata_indent + 2) "annotations:"
        print spaces(metadata_indent + 4) "kubectl.kubernetes.io/restartedAt: \"" value "\""
        inserted = 1
      }
      in_metadata = 0
    }
    { print }
    END {
      if (!inserted) {
        exit 3
      }
    }
  ' "$manifest" > "$output"
}

set_temporary_resource_name() {
  local manifest=$1
  local output=$2
  local temporary_name=$3
  awk -v temporary_name="$temporary_name" '
    /^metadata:[[:space:]]*$/ {
      top_metadata = 1
      print
      next
    }
    top_metadata && /^  name:[[:space:]]*/ {
      print "  name: " temporary_name
      replaced++
      top_metadata = 0
      next
    }
    top_metadata && /^[^[:space:]]/ {
      top_metadata = 0
    }
    { print }
    END {
      if (replaced != 1) {
        exit 3
      }
    }
  ' "$manifest" > "$output"
}

canonical_live_workload_spec() {
  local kind=$1
  local name=$2
  kubectl get "$kind/$name" -n "$NAMESPACE" -o json \
    | jq -cS -e --arg kind "$kind" -f "$CANONICALIZE_WORKLOAD_FILTER"
}

canonical_desired_workload_spec() {
  local kind=$1
  local manifest=$2
  kubectl create --dry-run=server -n "$NAMESPACE" -f "$manifest" -o json \
    | jq -cS -e --arg kind "$kind" -f "$CANONICALIZE_WORKLOAD_FILTER"
}

verify_manifest_and_live() {
  local manifest=$1
  local expected_migration=${2:-}
  local tmp_dir inventory rendered_claims rendered_runner rendered_worker_replicas
  local workloads kind name live_manifest live_inventory rendered_rows
  local container_type container_name rendered_image live_row live_image effective_image
  tmp_dir="$(mktemp -d)"
  inventory="$tmp_dir/inventory.tsv"
  if ! manifest_workload_inventory "$manifest" > "$inventory"; then
    rm -rf "$tmp_dir"
    return 1
  fi
  require_core_workloads "$inventory" || { rm -rf "$tmp_dir"; return 1; }
  while IFS=$'\t' read -r kind name container_type container_name rendered_image; do
    if ! is_digest_image "$rendered_image"; then
      echo "ERROR: current Helm rollback target leaves $kind/$name $container_type/$container_name on a mutable or non-sha256 image: $rendered_image" >&2
      rm -rf "$tmp_dir"
      return 1
    fi
  done < "$inventory"

  rendered_claims="$(claims_from_manifest < "$manifest")"
  if [ "$rendered_claims" != "false" ]; then
    echo "ERROR: current Helm revision (the rollback target) renders worker provisioning claims as $rendered_claims, not false." >&2
    rm -rf "$tmp_dir"
    return 1
  fi
  rendered_worker_replicas="$(rendered_worker_replicas_from_manifest "$manifest")"
  if [ "$rendered_worker_replicas" != "1" ]; then
    echo "ERROR: current Helm rollback target renders $rendered_worker_replicas workers, not exactly 1." >&2
    rm -rf "$tmp_dir"
    return 1
  fi
  rendered_runner="$(runner_image_from_manifest < "$manifest")"
  if ! is_digest_image "$rendered_runner"; then
    echo "ERROR: current Helm rollback target leaves RUNNER_IMAGE mutable: $rendered_runner" >&2
    rm -rf "$tmp_dir"
    return 1
  fi
  if ! validate_foundation_intent "$manifest" true false; then
    echo "ERROR: current Helm rollback target changes the claims/content-filter foundation intent." >&2
    rm -rf "$tmp_dir"
    return 1
  fi

  workloads="$(cut -f1,2 "$inventory" | sort -u)"
  : > "$tmp_dir/live-foundation.yaml"
  while IFS=$'\t' read -r kind name; do
    live_manifest="$tmp_dir/${kind}-${name}.yaml"
    if ! kubectl get "$kind/$name" -n "$NAMESPACE" -o yaml > "$live_manifest"; then
      echo "ERROR: rollback workload $kind/$name is missing from the live release." >&2
      rm -rf "$tmp_dir"
      return 1
    fi
    printf '%s\n---\n' "$(cat "$live_manifest")" >> "$tmp_dir/live-foundation.yaml"
    live_inventory="$tmp_dir/${kind}-${name}.inventory"
    manifest_workload_inventory "$live_manifest" > "$live_inventory"
    rendered_rows="$tmp_dir/${kind}-${name}.rendered"
    awk -F '\t' -v kind="$kind" -v name="$name" '
      $1 == kind && $2 == name { print }
    ' "$inventory" | sort > "$rendered_rows"
    if ! diff -u "$rendered_rows" <(sort "$live_inventory") > "$tmp_dir/${kind}-${name}.diff"; then
      echo "ERROR: live $kind/$name declared images differ from the Helm rollback target." >&2
      head -200 "$tmp_dir/${kind}-${name}.diff" >&2
      rm -rf "$tmp_dir"
      return 1
    fi
    while IFS=$'\t' read -r _ _ container_type container_name rendered_image; do
      live_row="$(
        awk -F '\t' \
          -v kind="$kind" \
          -v name="$name" \
          -v type="$container_type" \
          -v container="$container_name" \
          '$1 == kind && $2 == name && $3 == type && $4 == container { print; matches++ }
           END { if (matches != 1) exit 3 }' \
          "$live_inventory"
      )"
      live_image="$(printf '%s\n' "$live_row" | cut -f5)"
      effective_image="$(
        live_effective_image \
          "$kind" \
          "$name" \
          "$container_type" \
          "$container_name" \
          "$live_image" \
          "$live_manifest"
      )"
      if [ "$effective_image" != "$rendered_image" ]; then
        echo "ERROR: digest mismatch for $kind/$name $container_type/$container_name: live ImageID $effective_image, pinned baseline $rendered_image." >&2
        rm -rf "$tmp_dir"
        return 1
      fi
    done < "$rendered_rows"
  done <<< "$workloads"

  local live_worker live_claims live_replicas live_runner
  live_worker="$tmp_dir/Deployment-${RELEASE}-worker.yaml"
  live_claims="$(claims_from_manifest < "$live_worker")"
  if ! live_replicas="$(live_worker_replicas)"; then
    echo "ERROR: could not read an integer replica count of exactly 1 from the live worker Deployment." >&2
    rm -rf "$tmp_dir"
    return 1
  fi
  if [ "$live_claims" != "false" ] || [ "$live_replicas" != "1" ]; then
    echo "ERROR: live worker must have claims=false and replicas=1; got claims=$live_claims replicas=$live_replicas." >&2
    rm -rf "$tmp_dir"
    return 1
  fi
  live_runner="$(
    runner_image_from_manifest < "$tmp_dir/Deployment-${RELEASE}-engine.yaml"
  )"
  if [ "$live_runner" != "$rendered_runner" ]; then
    echo "ERROR: live engine RUNNER_IMAGE $live_runner differs from the pinned rollback target $rendered_runner." >&2
    rm -rf "$tmp_dir"
    return 1
  fi
  if ! validate_foundation_intent "$tmp_dir/live-foundation.yaml" false true; then
    echo "ERROR: live rollback workloads change the claims/content-filter foundation intent." >&2
    rm -rf "$tmp_dir"
    return 1
  fi
  if ! validate_synthetic_monitor_active "$tmp_dir/live-foundation.yaml"; then
    echo "ERROR: live rollback state has suspended the non-mutating API monitor." >&2
    rm -rf "$tmp_dir"
    return 1
  fi
  if ! workload_health "$inventory"; then
    rm -rf "$tmp_dir"
    return 1
  fi
  if ! require_clean_migration "$expected_migration"; then
    rm -rf "$tmp_dir"
    return 1
  fi
  rm -rf "$tmp_dir"
}

prepare_claims_baseline() {
  if [ -z "$BASELINE_CHART_DIR" ]; then
    echo "ERROR: --prepare-claims-baseline requires --baseline-chart-dir pointing at the currently deployed safe chart checkout." >&2
    return 64
  fi
  if [ ! -f "$BASELINE_CHART_DIR/Chart.yaml" ]; then
    echo "ERROR: baseline chart not found at $BASELINE_CHART_DIR" >&2
    return 1
  fi
  if [ ! -x "$PIN_BASELINE_SCRIPT" ]; then
    echo "ERROR: baseline post-renderer is missing or not executable at $PIN_BASELINE_SCRIPT" >&2
    return 1
  fi
  if ! command -v jq >/dev/null 2>&1; then
    echo "ERROR: jq is required to canonicalize live and server-defaulted workload specs." >&2
    return 1
  fi
  if [ ! -r "$CANONICALIZE_WORKLOAD_FILTER" ]; then
    echo "ERROR: workload canonicalization filter is missing or unreadable at $CANONICALIZE_WORKLOAD_FILTER" >&2
    return 1
  fi

  acquire_helm_release_lock

  local migration_before
  require_clean_migration
  migration_before=$VERIFIED_MIGRATION_STATE

  local deployed_revision
  if ! deployed_revision="$(latest_deployed_helm_revision)"; then
    echo "ERROR: no deployed Helm revision is available as the safe baseline source." >&2
    return 1
  fi

  local tmp_dir live_values candidate_unpinned candidate_manifest candidate_inventory
  local desired_map declared_map live_dir comparison_manifest
  tmp_dir="$(mktemp -d)"
  BASELINE_TMP_DIR=$tmp_dir
  live_values="$tmp_dir/live-values.yaml"
  candidate_unpinned="$tmp_dir/candidate-unpinned.yaml"
  candidate_manifest="$tmp_dir/candidate-manifest.yaml"
  candidate_inventory="$tmp_dir/candidate-inventory.tsv"
  desired_map="$tmp_dir/desired-images.tsv"
  declared_map="$tmp_dir/live-declared-images.tsv"
  live_dir="$tmp_dir/live"
  comparison_manifest="$tmp_dir/comparison-manifest.yaml"

  helm dependency build "$BASELINE_CHART_DIR"
  helm get values "$RELEASE" -n "$NAMESPACE" --revision "$deployed_revision" -o yaml > "$live_values"
  helm template "$RELEASE" "$BASELINE_CHART_DIR" \
    -n "$NAMESPACE" \
    -f "$live_values" \
    --set provisioning.workerClaimsEnabled=false \
    --skip-tests > "$candidate_unpinned"
  manifest_workload_inventory "$candidate_unpinned" > "$candidate_inventory"
  require_core_workloads "$candidate_inventory"
  build_live_image_maps \
    "$candidate_unpinned" \
    "$candidate_inventory" \
    "$desired_map" \
    "$declared_map" \
    "$live_dir"

  BASELINE_IMAGE_MAP="$desired_map" \
    BASELINE_RUNNER_IMAGE="$BASELINE_RUNNER_IMAGE" \
    "$PIN_BASELINE_SCRIPT" < "$candidate_unpinned" > "$candidate_manifest"
  validate_pinned_manifest "$candidate_manifest" "$desired_map" "$tmp_dir/pinned-inventory.tsv"

  BASELINE_IMAGE_MAP="$declared_map" \
    BASELINE_RUNNER_IMAGE="$LIVE_RUNNER_IMAGE" \
    "$PIN_BASELINE_SCRIPT" < "$candidate_unpinned" > "$comparison_manifest"

  echo "==> checking live resources against the safe chart (rollout timestamps are the only benign drift)"
  local workloads kind name workload_manifest annotated_manifest named_manifest rollout_annotation
  local live_spec desired_spec
  workloads="$(cut -f1,2 "$candidate_inventory" | sort -u)"
  while IFS=$'\t' read -r kind name; do
    workload_manifest="$tmp_dir/compare-${kind}-${name}.yaml"
    annotated_manifest="$tmp_dir/compare-${kind}-${name}-annotated.yaml"
    named_manifest="$tmp_dir/compare-${kind}-${name}-named.yaml"
    extract_workload_manifest "$comparison_manifest" "$kind" "$name" > "$workload_manifest"
    rollout_annotation="$(
      rollout_annotation_from_manifest < "$live_dir/${kind}-${name}.yaml"
    )"
    if [ -n "$rollout_annotation" ]; then
      set_rollout_annotation "$workload_manifest" "$annotated_manifest" "$rollout_annotation"
      echo "    $kind/$name: allowing live kubectl rollout annotation after digest equivalence proof"
    else
      cp "$workload_manifest" "$annotated_manifest"
    fi
    set_temporary_resource_name \
      "$annotated_manifest" \
      "$named_manifest" \
      "baseline-check-$RANDOM"
    if ! live_spec="$(canonical_live_workload_spec "$kind" "$name")"; then
      echo "ERROR: could not canonicalize live $kind/$name for direct drift comparison." >&2
      return 1
    fi
    if ! desired_spec="$(canonical_desired_workload_spec "$kind" "$named_manifest")"; then
      echo "ERROR: server dry-run could not canonicalize rendered $kind/$name for direct drift comparison." >&2
      return 1
    fi
    if [ "$live_spec" != "$desired_spec" ]; then
      printf '%s\n' "$live_spec" > "$tmp_dir/${kind}-${name}.live-spec"
      printf '%s\n' "$desired_spec" > "$tmp_dir/${kind}-${name}.desired-spec"
      echo "ERROR: live $kind/$name spec drifts from the server-defaulted safe chart beyond image pinning, claims=false, and its proven rollout annotation." >&2
      diff -u "$tmp_dir/${kind}-${name}.live-spec" "$tmp_dir/${kind}-${name}.desired-spec" | head -300 >&2 || true
      return 1
    fi
  done <<< "$workloads"

  workload_health "$candidate_inventory"
  require_clean_migration "$migration_before"

  echo "==> creating claims-disabled Helm baseline with every rendered workload and RUNNER_IMAGE pinned to live effective digests"
  if ! BASELINE_IMAGE_MAP="$desired_map" BASELINE_RUNNER_IMAGE="$BASELINE_RUNNER_IMAGE" \
      helm upgrade "$RELEASE" "$BASELINE_CHART_DIR" \
      --namespace "$NAMESPACE" \
      --reuse-values \
      --set provisioning.workerClaimsEnabled=false \
      --post-renderer "$PIN_BASELINE_SCRIPT" \
      --wait \
      --timeout "$TIMEOUT"; then
    echo "ERROR: baseline revision failed; reasserting the live claims override and refusing phase-1." >&2
    kubectl set env "deployment/$RELEASE-worker" \
      -n "$NAMESPACE" \
      WORKER_PROVISIONING_CLAIMS_ENABLED=false
    kubectl rollout status "deployment/$RELEASE-worker" -n "$NAMESPACE" --timeout=5m
    return 1
  fi

  verify_rollback_containment "$migration_before"
  require_clean_migration "$migration_before"
  rm -rf "$tmp_dir"
  BASELINE_TMP_DIR=
  release_helm_release_lock
  echo "==> immutable all-workload baseline complete; migration is unchanged and every affected workload, including the synthetic monitor, is healthy"
  echo "==> do not build, push, migrate, or deploy phase-1 images unless this revision remains the immediately previous successful Helm revision"
}

prepare_immutable_candidate() {
  local source_sha=$1
  if [ ! -x "$PIN_BASELINE_SCRIPT" ]; then
    echo "ERROR: image post-renderer is missing or not executable at $PIN_BASELINE_SCRIPT" >&2
    return 1
  fi
  if [ ! -x "$APPLY_EXACT_CANDIDATE_SCRIPT" ]; then
    echo "ERROR: exact-candidate post-renderer is missing or not executable at $APPLY_EXACT_CANDIDATE_SCRIPT" >&2
    return 1
  fi
  local tmp_dir candidate_render candidate_unpinned candidate_manifest candidate_inventory
  local resolved_images desired_map external_map live_dir server_manifest upgrade_hooks
  local ui_candidate_image
  tmp_dir="$(mktemp -d)"
  BASELINE_TMP_DIR=$tmp_dir
  candidate_render="$tmp_dir/candidate-render.yaml"
  candidate_unpinned="$tmp_dir/candidate-unpinned.yaml"
  candidate_manifest="$tmp_dir/candidate.yaml"
  candidate_inventory="$tmp_dir/candidate-unpinned-inventory.tsv"
  resolved_images="$tmp_dir/resolved-images.tsv"
  desired_map="$tmp_dir/candidate-images.tsv"
  external_map="$tmp_dir/external-images.tsv"
  live_dir="$tmp_dir/live"
  server_manifest="$tmp_dir/candidate-server.yaml"
  upgrade_hooks="$tmp_dir/candidate-upgrade-hooks.yaml"

  echo "==> resolving exact GHCR digests built by workflow run $PROVEN_WORKFLOW_RUN_ID for $source_sha"
  resolve_candidate_images "$source_sha" "$resolved_images"
  echo "==> resolving exact UI digest from workflow run $PROVEN_UI_WORKFLOW_RUN_ID for $UI_SOURCE_SHA"
  ui_candidate_image="$(
    resolve_ui_candidate_image \
      "$UI_SOURCE_SHA" \
      "$PROVEN_UI_WORKFLOW_RUN_ID" \
      "$PROVEN_UI_WORKFLOW_RUN_ATTEMPT"
  )"
  helm template "$RELEASE" . \
    -n "$NAMESPACE" \
    -f values.yaml \
    -f values.prod.yaml \
    --is-upgrade \
    --skip-tests \
    --set-string global.postgresql.auth.password=crucible-template-validation-only \
    > "$candidate_render"
  strip_helm_hooks "$candidate_render" "$candidate_unpinned"
  extract_upgrade_hooks "$candidate_render" "$upgrade_hooks"
  manifest_workload_inventory "$candidate_unpinned" > "$candidate_inventory"
  require_core_workloads "$candidate_inventory"
  build_candidate_image_maps \
    "$candidate_unpinned" \
    "$candidate_inventory" \
    "$resolved_images" \
    "$desired_map" \
    "$external_map" \
    "$live_dir" \
    "$ui_candidate_image"
  BASELINE_IMAGE_MAP="$desired_map" \
    BASELINE_RUNNER_IMAGE="$CANDIDATE_RUNNER_IMAGE" \
    "$PIN_BASELINE_SCRIPT" < "$candidate_unpinned" > "$candidate_manifest"
  validate_candidate_manifest \
    "$candidate_manifest" \
    "$desired_map" \
    "$tmp_dir/candidate-inventory.tsv"
  validate_upgrade_hooks "$upgrade_hooks" "$resolved_images" "$desired_map" "$tmp_dir"
  server_validate_candidate \
    "$candidate_manifest" \
    "$desired_map" \
    "$server_manifest" \
    "$tmp_dir/candidate-server.canonical"

  CANDIDATE_TMP_DIR=$tmp_dir
  CANDIDATE_MANIFEST=$candidate_manifest
  CANDIDATE_IMAGE_MAP=$desired_map
  CANDIDATE_EXTERNAL_IMAGE_MAP=$external_map
  CANDIDATE_RESOLVED_IMAGES=$resolved_images
  CANDIDATE_UPGRADE_HOOKS=$upgrade_hooks
  CANDIDATE_INVENTORY="$tmp_dir/candidate-inventory.tsv"
  CANDIDATE_SERVER_MANIFEST=$server_manifest
  CANDIDATE_SERVER_CANONICAL="$tmp_dir/candidate-server.canonical"
  CANDIDATE_UI_IMAGE=$ui_candidate_image
  CANDIDATE_SHA256="$(sha256sum "$candidate_manifest" | awk '{print $1}')"
}

if [ "$MODE" = verify ]; then
  verify_rollback_containment "" "$REQUIRED_ROLLBACK_REVISION"
  exit 0
fi

if [ "$MODE" = prepare ]; then
  prepare_claims_baseline
  exit $?
fi

if [ -z "$UI_SOURCE_SHA" ]; then
  echo "ERROR: production candidates require an explicit --ui-source-sha; refusing to preserve or select UI implicitly." >&2
  exit 64
fi

# Prove the stored rollback manifest before source or candidate work, without
# requiring a temporary live claims override to be paused for --dry-run.
verify_rollback_manifest_safety
ROLLBACK_BASELINE_REVISION=$STATIC_BASELINE_REVISION

# Refuse a dirty tree before pull so git cannot merge local state into the
# candidate, then prove the exact source identity again after pull.
require_clean_source_tree
if [ "$DO_PULL" = true ]; then
  echo "==> git pull"
  git -C "$REPOSITORY_ROOT" pull --ff-only
fi
CANDIDATE_SOURCE_SHA="$(require_trusted_source_identity)"
prove_successful_source_build "$CANDIDATE_SOURCE_SHA"
echo "==> trusted source commit $CANDIDATE_SOURCE_SHA has successful required image builds"
prove_successful_ui_build "$UI_SOURCE_SHA"
CANDIDATE_UI_WORKFLOW_RUN_ID=$PROVEN_UI_WORKFLOW_RUN_ID
CANDIDATE_UI_WORKFLOW_RUN_ATTEMPT=$PROVEN_UI_WORKFLOW_RUN_ATTEMPT
echo "==> trusted UI source commit $UI_SOURCE_SHA has successful test and image builds"

cd "$CHART_DIR"

echo "==> helm dep build (uses Chart.lock for pinned subchart versions)"
helm dep build
prepare_immutable_candidate "$CANDIDATE_SOURCE_SHA"

if [ "$DO_DRY_RUN" = true ]; then
  live_render="$CANDIDATE_TMP_DIR/live-render.yaml"
  helm get manifest "$RELEASE" -n "$NAMESPACE" > "$live_render"
  echo "==> exact server-validated production candidate ($CANDIDATE_SOURCE_SHA)"
  cat "$CANDIDATE_MANIFEST"
  echo "==> dry-run diff vs current release"
  if diff -u "$live_render" "$CANDIDATE_MANIFEST"; then
    echo "==> no diff vs live"
  else
    echo "==> diff above; re-run without --dry-run to apply"
  fi
  exit 0
fi

# Candidate provenance, rendering, and the first server validation happen
# before claims pause. Only a real apply acquires the lock and mutates live
# claims; --dry-run remains useful while a temporary claims=true override exists.
acquire_helm_release_lock
verify_rollback_manifest_safety
if [ "$STATIC_BASELINE_REVISION" != "$ROLLBACK_BASELINE_REVISION" ]; then
  echo "ERROR: Helm revision changed from baseline $ROLLBACK_BASELINE_REVISION to $STATIC_BASELINE_REVISION before claims pause." >&2
  exit 1
fi
pause_live_provisioning_claims
enforce_synthetic_rollback_containment
require_no_active_jobs
verify_rollback_containment "" "$REQUIRED_ROLLBACK_REVISION"
ROLLBACK_BASELINE_MIGRATION=$VERIFIED_MIGRATION_STATE
if [ "$VERIFIED_BASELINE_REVISION" != "$ROLLBACK_BASELINE_REVISION" ]; then
  echo "ERROR: Helm revision changed from stored baseline $ROLLBACK_BASELINE_REVISION to $VERIFIED_BASELINE_REVISION while pausing claims." >&2
  exit 1
fi

if [ "$(require_trusted_source_identity "$CANDIDATE_SOURCE_SHA")" != "$CANDIDATE_SOURCE_SHA" ]; then
  echo "ERROR: source identity changed before application upgrade." >&2
  exit 1
fi
prove_successful_ui_build "$UI_SOURCE_SHA"
if [ "$PROVEN_UI_WORKFLOW_RUN_ID" != "$CANDIDATE_UI_WORKFLOW_RUN_ID" ] ||
   [ "$PROVEN_UI_WORKFLOW_RUN_ATTEMPT" != "$CANDIDATE_UI_WORKFLOW_RUN_ATTEMPT" ]; then
  echo "ERROR: proven UI workflow identity changed before application upgrade." >&2
  exit 1
fi
if [ "$(
  resolve_ui_candidate_image \
    "$UI_SOURCE_SHA" \
    "$CANDIDATE_UI_WORKFLOW_RUN_ID" \
    "$CANDIDATE_UI_WORKFLOW_RUN_ATTEMPT"
)" != "$CANDIDATE_UI_IMAGE" ]; then
  echo "ERROR: proven UI image changed before application upgrade." >&2
  exit 1
fi
verify_rollback_containment \
  "$ROLLBACK_BASELINE_MIGRATION" \
  "$REQUIRED_ROLLBACK_REVISION"
if [ "$VERIFIED_BASELINE_REVISION" != "$ROLLBACK_BASELINE_REVISION" ]; then
  echo "ERROR: Helm revision changed from baseline $ROLLBACK_BASELINE_REVISION to $VERIFIED_BASELINE_REVISION before the application upgrade." >&2
  echo "Re-establish and re-verify the immutable all-workload baseline; refusing phase-1." >&2
  exit 1
fi
verify_external_candidate_images \
  "$CANDIDATE_EXTERNAL_IMAGE_MAP" \
  "$CANDIDATE_TMP_DIR/external-recheck"
validate_upgrade_hooks \
  "$CANDIDATE_UPGRADE_HOOKS" \
  "$CANDIDATE_RESOLVED_IMAGES" \
  "$CANDIDATE_IMAGE_MAP" \
  "$CANDIDATE_TMP_DIR/upgrade-hook-final"
server_validate_candidate \
  "$CANDIDATE_MANIFEST" \
  "$CANDIDATE_IMAGE_MAP" \
  "$CANDIDATE_TMP_DIR/candidate-server-final.yaml" \
  "$CANDIDATE_TMP_DIR/candidate-server-final.canonical"
if ! diff -u \
    "$CANDIDATE_SERVER_CANONICAL" \
    "$CANDIDATE_TMP_DIR/candidate-server-final.canonical"; then
  echo "ERROR: final server-defaulted candidate object set changed after its initial validation." >&2
  exit 1
fi
if [ "$(sha256sum "$CANDIDATE_MANIFEST" | awk '{print $1}')" != "$CANDIDATE_SHA256" ]; then
  echo "ERROR: exact candidate manifest changed after validation." >&2
  exit 1
fi
require_no_active_jobs

echo "==> helm upgrade $RELEASE with exact digest-pinned candidate (atomic, timeout=$TIMEOUT)"
set +e
CANDIDATE_IMAGE_MAP="$CANDIDATE_IMAGE_MAP" \
CANDIDATE_RUNNER_IMAGE="$CANDIDATE_RUNNER_IMAGE" \
EXPECTED_CANDIDATE_MANIFEST="$CANDIDATE_MANIFEST" \
EXPECTED_CANDIDATE_SHA256="$CANDIDATE_SHA256" \
helm upgrade "$RELEASE" . \
  --namespace "$NAMESPACE" \
  --install \
  -f values.yaml \
  -f values.prod.yaml \
  --post-renderer "$APPLY_EXACT_CANDIDATE_SCRIPT" \
  --atomic \
  --timeout "$TIMEOUT"
helm_upgrade_status=$?
set -e
if [ "$helm_upgrade_status" -ne 0 ]; then
  if contain_failed_atomic_upgrade; then
    release_helm_release_lock
  else
    HELM_RELEASE_LOCK_PRESERVE=true
    echo "ERROR: atomic upgrade failed and rollback containment could not be proven. The release lock is intentionally retained; manual intervention is required." >&2
  fi
  exit "$helm_upgrade_status"
fi

if ! verify_deployed_candidate; then
  HELM_RELEASE_LOCK_PRESERVE=true
  echo "ERROR: Helm reported success but exact candidate containment failed. The release lock is intentionally retained; manual intervention is required." >&2
  exit 1
fi
release_helm_release_lock
echo "==> deployed exact source $CANDIDATE_SOURCE_SHA with immutable workload and RUNNER_IMAGE digests"
