#!/usr/bin/env bash
# Materialise ./third_party/litefs: upstream LiteFS at the pinned tag with the
# patches in hack/litefs applied. go.mod points the litefs module at it.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DEST="${ROOT}/third_party/litefs"
TAG="${LITEFS_TAG:-v0.5.14}"
MARKER="${DEST}/.clusterplex-patched"

patches_applied() {
  local p
  for p in "${ROOT}"/hack/litefs/*.patch; do
    git -C "${DEST}" apply --check --reverse "$p" >/dev/null 2>&1 || return 1
  done
}

if [ -d "${DEST}" ]; then
  if [ -e "${MARKER}" ] && [ "$(cat "${MARKER}")" = "${TAG}" ]; then
    exit 0
  fi
  if patches_applied; then
    echo "${TAG}" > "${MARKER}"
    exit 0
  fi
  echo "third_party/litefs exists but does not carry the hack/litefs patches; move it aside and rerun" >&2
  exit 1
fi

echo "fetching superfly/litefs ${TAG} into third_party/litefs" >&2
mkdir -p "$(dirname "${DEST}")"
git clone --quiet --depth 1 --branch "${TAG}" https://github.com/superfly/litefs "${DEST}"
for p in "${ROOT}"/hack/litefs/*.patch; do
  git -C "${DEST}" apply "$p"
done
echo "${TAG}" > "${MARKER}"
