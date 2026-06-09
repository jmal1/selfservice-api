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

for arg in "$@"; do
  case "$arg" in
    --no-pull) DO_PULL=false ;;
    --dry-run) DO_DRY_RUN=true ;;
    *) echo "Unknown arg: $arg" >&2; exit 64 ;;
  esac
done

if [ -z "${KUBECONFIG:-}" ]; then
  if [ -f /etc/rancher/k3s/k3s.yaml ]; then
    export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  fi
fi

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

echo "==> pod status:"
kubectl get pods -n "$NAMESPACE" -o wide
