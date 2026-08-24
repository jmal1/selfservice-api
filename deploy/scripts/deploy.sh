#!/bin/bash
# Deploy the selfservice helm chart to the production K3s cluster.
#
# This script is intended to run on k3sv01 from a sparse-checkout of this
# repository where ./deploy/helm/selfservice is the chart root.
#
# Usage:
#   ./deploy.sh             # standard deploy (git pull + helm upgrade)
#   ./deploy.sh --no-pull   # skip git pull (use the working tree as-is)
#   ./deploy.sh --dry-run   # render + diff but don't upgrade
#   ./deploy.sh --verify-rollback-containment
#   ./deploy.sh --prepare-claims-baseline --baseline-chart-dir /path/to/safe/chart
#
# Required on the deploy host:
#   - kubectl with KUBECONFIG pointing at k3s (typically /etc/rancher/k3s/k3s.yaml)
#   - helm 3.14+
#   - jq
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
CANONICALIZE_WORKLOAD_FILTER="$SCRIPT_DIR/canonicalize-workload-spec.jq"
HELM_RELEASE_LOCK="$RELEASE-phase1-deploy-lock"
HELM_RELEASE_LOCK_HELD=false
HELM_RELEASE_LOCK_HOLDER=
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
  release_helm_release_lock
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

worker_replicas_from_manifest() {
  local manifest=$1
  extract_workload_manifest "$manifest" Deployment "$RELEASE-worker" \
    | awk '
        /^  replicas:[[:space:]]*/ {
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

manifest_workload_inventory() {
  local manifest=$1
  awk '
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
        value == "Job"
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

is_digest_image() {
  [[ "$1" =~ ^[^[:space:]]+@sha256:[0-9a-fA-F]{64}$ ]]
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
    repository="$(image_repository "$declared_image")"
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

verify_rollback_containment() {
  local expected_migration=${1:-}
  local revision status manifest
  if ! read -r revision status <<< "$(latest_helm_revision_record)"; then
    echo "ERROR: could not determine the latest Helm revision and status." >&2
    return 1
  fi
  if [ "$status" != "deployed" ]; then
    echo "ERROR: latest Helm revision $revision status is $status, not deployed; it cannot be a rollback baseline." >&2
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
      if [ "$(image_repository "$candidate_image")" != "$(image_repository "$live_image")" ]; then
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
  if [ "$(image_repository "$candidate_runner")" != "$(image_repository "$BASELINE_RUNNER_IMAGE")" ] ||
     [ "$(image_repository "$LIVE_RUNNER_IMAGE")" != "$(image_repository "$BASELINE_RUNNER_IMAGE")" ]; then
    echo "ERROR: engine RUNNER_IMAGE and the live runner-image-warmer do not identify the same runner repository." >&2
    return 1
  fi
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
  rendered_worker_replicas="$(worker_replicas_from_manifest "$manifest")"
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

  workloads="$(cut -f1,2 "$inventory" | sort -u)"
  while IFS=$'\t' read -r kind name; do
    live_manifest="$tmp_dir/${kind}-${name}.yaml"
    if ! kubectl get "$kind/$name" -n "$NAMESPACE" -o yaml > "$live_manifest"; then
      echo "ERROR: rollback workload $kind/$name is missing from the live release." >&2
      rm -rf "$tmp_dir"
      return 1
    fi
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
  live_replicas="$(worker_replicas_from_manifest "$live_worker")"
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
    --set provisioning.workerClaimsEnabled=false > "$candidate_unpinned"
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

if [ "$MODE" = verify ]; then
  verify_rollback_containment
  exit 0
fi

if [ "$MODE" = prepare ]; then
  prepare_claims_baseline
  exit $?
fi

# The current revision becomes Helm's rollback target for the phase-1 upgrade.
# Verify it before pulling new code or touching images so an atomic rollback
# cannot restore claims-enabled worker behavior or mutable application images.
verify_rollback_containment
ROLLBACK_BASELINE_REVISION=$VERIFIED_BASELINE_REVISION
ROLLBACK_BASELINE_MIGRATION=$VERIFIED_MIGRATION_STATE

# Refuse to deploy if the working tree is dirty.
if [ "$DO_PULL" = true ]; then
  cd "$SCRIPT_DIR"
  if ! git diff --quiet || ! git diff --cached --quiet; then
    echo "ERROR: working tree has uncommitted changes. Commit or stash first." >&2
    git status --short >&2
    exit 1
  fi
  echo "==> git pull"
  git pull --ff-only
fi

cd "$CHART_DIR"

echo "==> helm dep build (uses Chart.lock for pinned subchart versions)"
helm dep build

if [ "$DO_DRY_RUN" = true ]; then
  echo "==> dry-run diff vs current release"
  helm template "$RELEASE" . -n "$NAMESPACE" -f values.yaml -f values.prod.yaml > /tmp/new-render.yaml
  helm get manifest "$RELEASE" -n "$NAMESPACE" > /tmp/live-render.yaml
  if diff -u /tmp/live-render.yaml /tmp/new-render.yaml; then
    echo "==> no diff vs live"
  else
    echo "==> diff above; re-run without --dry-run to apply"
  fi
  exit 0
fi

# A concurrent Helm revision would displace the proven baseline from the
# immediately previous slot used by --atomic. Re-run the live proof after pull
# and dependency resolution, but compare against the pre-pull migration state
# because the new checkout may legitimately contain the phase-1 migration.
acquire_helm_release_lock
verify_rollback_containment "$ROLLBACK_BASELINE_MIGRATION"
if [ "$VERIFIED_BASELINE_REVISION" != "$ROLLBACK_BASELINE_REVISION" ]; then
  echo "ERROR: Helm revision changed from baseline $ROLLBACK_BASELINE_REVISION to $VERIFIED_BASELINE_REVISION before the application upgrade." >&2
  echo "Re-establish and re-verify the immutable all-workload baseline; refusing phase-1." >&2
  exit 1
fi

echo "==> helm upgrade $RELEASE (atomic, timeout=$TIMEOUT)"
helm upgrade "$RELEASE" . \
  --namespace "$NAMESPACE" \
  --install \
  -f values.yaml \
  -f values.prod.yaml \
  --atomic \
  --timeout "$TIMEOUT"

echo "==> revision:"
helm list -n "$NAMESPACE" -o table | grep "$RELEASE"

# Every workload here runs a floating `:latest` tag, so a rebuilt image leaves
# the pod spec byte-identical and `helm upgrade` reports success without
# restarting anything. That has bitten this deploy three times: `helm status:
# deployed` while the old code kept serving. Restarting explicitly is the only
# way `deploy.sh` actually deploys.
#
# The list is explicit rather than label-selected on purpose. The obvious
# selector (app.kubernetes.io/instance=$RELEASE) also matches the postgresql and
# nats subcharts, and this script must never bounce the database. The labels are
# not consistent enough to filter on either — selfservice-engine carries no
# instance label and selfservice-ui carries no component label. An explicit list
# also fails loudly if a workload is renamed, instead of silently restarting
# nothing.
#
# The runner-image warmer matters twice over: if it is not restarted it keeps a
# *stale* runner image resident, which is worse than having no warmer at all.
CHART_DEPLOYMENTS=(
  "$RELEASE-api"
  "$RELEASE-engine"
  "$RELEASE-ui"
  "$RELEASE-worker"
)
CHART_DAEMONSETS=(
  "$RELEASE-runner-image-warmer"
)

echo "==> restarting workloads (floating :latest tags do not roll on their own)"
for d in "${CHART_DEPLOYMENTS[@]}"; do
  kubectl rollout restart "deployment/$d" -n "$NAMESPACE"
done
for ds in "${CHART_DAEMONSETS[@]}"; do
  # Skipped rather than fatal: the warmer is optional (engine.runnerImageWarmer.enabled).
  kubectl get "daemonset/$ds" -n "$NAMESPACE" >/dev/null 2>&1 \
    && kubectl rollout restart "daemonset/$ds" -n "$NAMESPACE" \
    || echo "    (daemonset/$ds not present, skipping)"
done

echo "==> waiting for rollouts"
for d in "${CHART_DEPLOYMENTS[@]}"; do
  kubectl rollout status "deployment/$d" -n "$NAMESPACE" --timeout=5m
done

echo "==> pod status:"
kubectl get pods -n "$NAMESPACE" -o wide
release_helm_release_lock
