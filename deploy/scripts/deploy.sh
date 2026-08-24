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

worker_image_from_manifest() {
  awk '
    /^[[:space:]]*- name: provision-worker[[:space:]]*$/ {
      worker = 1
      next
    }
    worker && /^[[:space:]]*image:[[:space:]]*/ {
      matches++
      value = $0
      sub(/^[[:space:]]*image:[[:space:]]*/, "", value)
      gsub(/^["'"'"']|["'"'"']$/, "", value)
      worker = 0
    }
    END {
      if (matches != 1 || value == "") {
        exit 3
      }
      print value
    }
  '
}

live_worker_claims() {
  kubectl get "deployment/$RELEASE-worker" \
    -n "$NAMESPACE" \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="provision-worker")].env[?(@.name=="WORKER_PROVISIONING_CLAIMS_ENABLED")].value}'
}

live_worker_deployment_image() {
  kubectl get "deployment/$RELEASE-worker" \
    -n "$NAMESPACE" \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="provision-worker")].image}'
}

live_worker_image_digest() {
  local image_ids
  image_ids="$(
    kubectl get pods \
      -n "$NAMESPACE" \
      -l app.kubernetes.io/component=provision-worker \
      -o jsonpath='{range .items[*].status.containerStatuses[?(@.name=="provision-worker")]}{.imageID}{"\n"}{end}' \
      | sed -e 's#^docker-pullable://##' -e 's#^docker://##' -e '/^[[:space:]]*$/d' \
      | sort -u
  )"
  if [ "$(printf '%s\n' "$image_ids" | sed -e '/^[[:space:]]*$/d' | wc -l | tr -d ' ')" != "1" ]; then
    echo "ERROR: live provision-worker pods do not report one stable image digest." >&2
    return 1
  fi
  if [[ ! "$image_ids" =~ ^[^[:space:]]+@sha256:[0-9a-fA-F]{64}$ ]]; then
    echo "ERROR: live provision-worker image ID is not an immutable sha256 digest: $image_ids" >&2
    return 1
  fi
  printf '%s' "$image_ids"
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
  local revision status manifest rendered_claims rendered_image live_claims live_image
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
  if ! rendered_claims="$(printf '%s\n' "$manifest" | claims_from_manifest)"; then
    echo "ERROR: current Helm revision does not render exactly one WORKER_PROVISIONING_CLAIMS_ENABLED value." >&2
    return 1
  fi
  if [ "$rendered_claims" != "false" ]; then
    echo "ERROR: current Helm revision (the rollback target) renders worker provisioning claims as $rendered_claims, not false." >&2
    echo "Establish a claims-disabled baseline revision before attempting the phase-1 upgrade." >&2
    return 1
  fi

  if ! rendered_image="$(printf '%s\n' "$manifest" | worker_image_from_manifest)"; then
    echo "ERROR: current Helm revision does not render exactly one provision-worker image." >&2
    return 1
  fi
  if [[ ! "$rendered_image" =~ ^[^[:space:]]+@sha256:[0-9a-fA-F]{64}$ ]]; then
    echo "ERROR: current Helm rollback target does not pin provision-worker to an immutable sha256 digest: $rendered_image" >&2
    return 1
  fi

  live_claims="$(live_worker_claims)"
  if [ "$live_claims" != "false" ]; then
    echo "ERROR: live worker deployment reports WORKER_PROVISIONING_CLAIMS_ENABLED=$live_claims, not false." >&2
    return 1
  fi
  live_image="$(live_worker_deployment_image)"
  if [ "$live_image" != "$rendered_image" ]; then
    echo "ERROR: live worker image $live_image differs from Helm rollback target $rendered_image." >&2
    return 1
  fi
  echo "==> rollback containment verified: deployed revision $revision has claims false and worker image $rendered_image"
}

extract_worker_manifest() {
  awk '
    /^# Source: selfservice\/templates\/worker-deployment.yaml$/ {
      capture = 1
    }
    capture && seen && /^---$/ {
      exit
    }
    capture {
      print
      seen = 1
    }
  '
}

normalize_worker_baseline() {
  awk '
    /^[[:space:]]*- name: WORKER_PROVISIONING_CLAIMS_ENABLED[[:space:]]*$/ {
      print
      if (getline <= 0) {
        exit 2
      }
      sub(/value:.*/, "value: \"<claims-disabled-baseline>\"")
      print
      next
    }
    /^[[:space:]]*image:[[:space:]]*/ {
      sub(/image:.*/, "image: \"<safe-worker-digest>\"")
      print
      next
    }
    { print }
  '
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

  local live_claims safe_worker_image
  live_claims="$(live_worker_claims)"
  if [ "$live_claims" != "false" ]; then
    echo "ERROR: live worker claims must already be false before creating the durable Helm baseline." >&2
    return 1
  fi
  safe_worker_image="$(live_worker_image_digest)"

  local deployed_revision
  if ! deployed_revision="$(latest_deployed_helm_revision)"; then
    echo "ERROR: no deployed Helm revision is available as the safe baseline source." >&2
    return 1
  fi

  local tmp_dir live_values live_manifest candidate_unpinned candidate_manifest live_worker candidate_worker post_renderer
  tmp_dir="$(mktemp -d)"
  live_values="$tmp_dir/live-values.yaml"
  live_manifest="$tmp_dir/live-manifest.yaml"
  candidate_unpinned="$tmp_dir/candidate-unpinned.yaml"
  candidate_manifest="$tmp_dir/candidate-manifest.yaml"
  live_worker="$tmp_dir/live-worker.yaml"
  candidate_worker="$tmp_dir/candidate-worker.yaml"
  post_renderer="$tmp_dir/pin-safe-worker-image.sh"

  cat > "$post_renderer" <<'EOF'
#!/bin/bash
set -euo pipefail
: "${SAFE_WORKER_IMAGE:?SAFE_WORKER_IMAGE is required}"
awk -v image="$SAFE_WORKER_IMAGE" '
  /^[[:space:]]*- name: provision-worker[[:space:]]*$/ {
    worker = 1
    print
    next
  }
  worker && /^[[:space:]]*image:[[:space:]]*/ {
    sub(/image:.*/, "image: \"" image "\"")
    print
    worker = 0
    replaced++
    next
  }
  { print }
  END {
    if (replaced != 1) {
      exit 42
    }
  }
'
EOF
  chmod 700 "$post_renderer"

  helm dependency build "$BASELINE_CHART_DIR"
  helm get values "$RELEASE" -n "$NAMESPACE" --revision "$deployed_revision" -o yaml > "$live_values"
  helm get manifest "$RELEASE" -n "$NAMESPACE" --revision "$deployed_revision" > "$live_manifest"
  helm template "$RELEASE" "$BASELINE_CHART_DIR" \
    -n "$NAMESPACE" \
    -f "$live_values" \
    --set provisioning.workerClaimsEnabled=false > "$candidate_unpinned"
  SAFE_WORKER_IMAGE="$safe_worker_image" "$post_renderer" \
    < "$candidate_unpinned" > "$candidate_manifest"

  extract_worker_manifest < "$live_manifest" | normalize_worker_baseline > "$live_worker"
  extract_worker_manifest < "$candidate_manifest" | normalize_worker_baseline > "$candidate_worker"
  if [ ! -s "$live_worker" ] || [ ! -s "$candidate_worker" ]; then
    echo "ERROR: could not extract the worker deployment from both Helm manifests." >&2
    rm -rf "$tmp_dir"
    return 1
  fi
  if ! diff -u "$live_worker" "$candidate_worker" > "$tmp_dir/worker.diff"; then
    echo "ERROR: baseline chart changes the worker deployment beyond disabling claims." >&2
    echo "Use the exact currently deployed safe chart checkout; refusing to create a mixed baseline." >&2
    head -200 "$tmp_dir/worker.diff" >&2
    rm -rf "$tmp_dir"
    return 1
  fi

  echo "==> creating claims-disabled Helm baseline pinned to $safe_worker_image"
  if ! SAFE_WORKER_IMAGE="$safe_worker_image" helm upgrade "$RELEASE" "$BASELINE_CHART_DIR" \
      --namespace "$NAMESPACE" \
      --reuse-values \
      --set provisioning.workerClaimsEnabled=false \
      --post-renderer "$post_renderer" \
      --wait \
      --timeout "$TIMEOUT"; then
    echo "ERROR: baseline revision failed; reasserting the live claims override and refusing phase-1." >&2
    kubectl set env "deployment/$RELEASE-worker" \
      -n "$NAMESPACE" \
      WORKER_PROVISIONING_CLAIMS_ENABLED=false
    kubectl rollout status "deployment/$RELEASE-worker" -n "$NAMESPACE" --timeout=5m
    rm -rf "$tmp_dir"
    return 1
  fi
  kubectl rollout status "deployment/$RELEASE-worker" -n "$NAMESPACE" --timeout=5m
  verify_rollback_containment
  rm -rf "$tmp_dir"
  echo "==> baseline complete; do not build, push, migrate, or deploy phase-1 images until this step succeeds"
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
# cannot restore claims-enabled worker behavior.
verify_rollback_containment

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
