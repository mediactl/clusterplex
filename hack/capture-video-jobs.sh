#!/bin/sh
# capture-video-jobs.sh records Plex's own video conversions on plex-0, for
# the GPU pool's fixtures (docs/superpowers/plans/2026-10-26-gpu-video-transcode-pool.md,
# Task 1): every Plex Transcoder's argv, cwd and allowed environment, its
# process state as PMS paces it, the transcoder's requests to PMS, and PMS's
# throttle log lines. Everything leaves the pod redacted.
#
#   hack/capture-video-jobs.sh <seconds> [watch|<api mode>] [out] [trace|light|notrace]
#
# watch records whatever plays meanwhile (a tune on the iPhone); every other
# mode starts a conversion through Plex's API as a client would and holds it
# for <seconds> (the inner script's api lists them). trace straces the
# transcoder's socket and file calls, light its socket calls alone (cheap
# enough not to slow it), notrace nothing. NAMESPACE selects plex-0's
# namespace.
set -eu

secs=${1:?usage: capture-video-jobs.sh <seconds> [mode] [out]}
mode=${2:-watch}
out=${3:-capture-$mode-$(date +%Y%m%d-%H%M%S).log}
trace=${4:-trace}
here=$(cd "$(dirname "$0")" && pwd)
ns=${NAMESPACE:+-n $NAMESPACE}

spec=$(mktemp)
trap 'rm -f "$spec"' EXIT
printf '%s' '{"securityContext":{"runAsUser":0,"runAsGroup":0,"runAsNonRoot":false,"allowPrivilegeEscalation":false}}' >"$spec"

# shellcheck disable=SC2086
kubectl $ns debug plex-0 -q -i --image=nicolaka/netshoot:v0.13 --target=plex --profile=general \
	--custom="$spec" -- sh -s -- "$secs" "$mode" "$trace" <"$here/capture-video-jobs.inner.sh" >/dev/null 2>&1 || true

# kubectl debug -i returns once it has sent the script; the container runs on.
newest() {
	# shellcheck disable=SC2086
	kubectl $ns get pod plex-0 -o json | jq -r '.status.ephemeralContainerStatuses
		| sort_by(.state.running.startedAt // .state.terminated.startedAt) | last | .name'
}
state() {
	# shellcheck disable=SC2086
	kubectl $ns get pod plex-0 -o json |
		jq -r --arg c "$1" '.status.ephemeralContainerStatuses[] | select(.name == $c) | .state | keys[0]'
}
c=$(newest)
while [ "$(state "$c")" != terminated ]; do sleep 5; done
# shellcheck disable=SC2086
kubectl $ns logs plex-0 -c "$c" >"$out"
echo "$out"
