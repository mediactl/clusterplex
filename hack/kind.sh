#!/usr/bin/env bash
set -euo pipefail
export PATH=$PATH:$(go env GOPATH)/bin

CLUSTER_NAME="${KIND_CLUSTER_NAME:-cluster-plex}"
NAMESPACE="media"
IMG="${IMG:-ghcr.io/mediactl/cluster-plex:dev}"
CONTEXT="kind-${CLUSTER_NAME}"
# cloud-provider-kind gives LoadBalancer Services a real address. Without it
# they sit at <pending> for ever, and a Gateway in front of them never reaches
# Programmed=True. https://kind.sigs.k8s.io/docs/user/loadbalancer/
LB_IMAGE="registry.k8s.io/cloud-provider-kind/cloud-controller-manager:v0.11.1"
LB_NAME="cloud-provider-kind"

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

# It runs beside the cluster rather than in it: it needs the Docker socket to
# hand out addresses on the kind network, and it serves every kind cluster on
# that network at once, so one container is enough however many exist.
ensure_loadbalancer() {
  if docker ps --filter "name=^${LB_NAME}$" --format '{{.Names}}' | grep -qx "${LB_NAME}"; then
    log "load balancer '${LB_NAME}' already running"
    return
  fi
  log "starting the load balancer '${LB_NAME}'"
  docker rm -f "${LB_NAME}" >/dev/null 2>&1 || true
  docker run -d --name "${LB_NAME}" --network kind --restart unless-stopped \
    -v /var/run/docker.sock:/var/run/docker.sock "${LB_IMAGE}" >/dev/null
}

cmd_up() {
  need kind
  need kubectl
  need docker
  create_cluster
  ensure_namespace
  ensure_loadbalancer
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
  lb)   need docker; ensure_loadbalancer ;;
  load) cmd_load ;;
  down) cmd_down ;;
  *)    die "unknown subcommand" ;;
esac
