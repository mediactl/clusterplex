#!/usr/bin/env bash
set -euo pipefail
export PATH=$PATH:$(go env GOPATH)/bin

CLUSTER_NAME="${KIND_CLUSTER_NAME:-cluster-plex}"
NAMESPACE="media"
IMG="${IMG:-ghcr.io/mediactl/cluster-plex:dev}"
CONTEXT="kind-${CLUSTER_NAME}"

log()  { printf '==> %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"; }

cluster_exists() {
  kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"
}

kc() { kubectl --context "${CONTEXT}" "$@"; }

create_cluster() {
  if cluster_exists; then
    log "kind cluster '${CLUSTER_NAME}' already exists"
    return
  fi
  log "creating kind cluster '${CLUSTER_NAME}'"
  kind create cluster --name "${CLUSTER_NAME}" --wait 120s
}

ensure_namespace() {
  kc create namespace "${NAMESPACE}" --dry-run=client -o yaml | kc apply -f -
}

cmd_up() {
  need kind
  need kubectl
  create_cluster
  ensure_namespace
}

cmd_load() {
  need kind
  need docker
  cluster_exists || die "cluster not found"
  log "loading ${IMG}"
  kind load docker-image --name "${CLUSTER_NAME}" "${IMG}"
}

cmd_down() {
  need kind
  if cluster_exists; then
    kind delete cluster --name "${CLUSTER_NAME}"
  fi
}

case "${1:-}" in
  up)   cmd_up ;;
  load) cmd_load ;;
  down) cmd_down ;;
  *)    die "unknown subcommand" ;;
esac
