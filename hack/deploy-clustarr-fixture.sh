#!/usr/bin/env bash
# Deploys k8s/overlays/kind-clustarr: the kind overlay plus stand-ins for a
# clustarr install, including catalog.clustarr.io CRDs.
#
# It refuses any cluster holding a catalog.clustarr.io CRD that is not
# labelled as this fixture, because that is a real clustarr: applying the
# overlay would replace its CRDs, and deleting it would delete its catalog.
# kind-cluster-plex runs the owner's clustarr, so this refuses it.
#
#   hack/deploy-clustarr-fixture.sh [--check]
#
# KUBE_CONTEXT picks the context (default: the current one). --check only
# reports whether the context is safe.
set -euo pipefail

ctx="${KUBE_CONTEXT:-$(kubectl config current-context)}"
real="$(kubectl --context "${ctx}" get crd -l '!clusterplex.mediactl.io/e2e-fixture' -o name \
  | grep -E '\.catalog\.clustarr\.io$' || true)"
if [[ -n "${real}" ]]; then
  echo "refusing: context ${ctx} runs a real clustarr (CRDs not labelled as the e2e fixture):" >&2
  echo "${real}" >&2
  exit 1
fi
if [[ "${1:-}" == "--check" ]]; then
  echo "ok: context ${ctx} holds no real clustarr"
  exit 0
fi
kubectl --context "${ctx}" apply -k "$(dirname "$0")/../k8s/overlays/kind-clustarr"
