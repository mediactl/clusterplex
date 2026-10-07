#!/usr/bin/env bash
# Records PMS's Live TV calls against a real Plex (docs/livetv-pms-calls.md):
# a throwaway HDHomeRun probe in the media namespace, one device and one DVR
# made, sized and removed, every answer written token-scrubbed to
# pkg/plex/api/testdata/livetv. It creates and deletes a DVR on that Plex,
# so run it only with the owner's OK.
#
#   CTX=kind-cluster-plex hack/record-livetv.sh
#
# Writes go to the lease holder's own PMS, port-forwarded: plex-main balances
# across pods, and a pod that does not hold the lease answers with its own
# view. The token is the server's, from the Secret the lease holder keeps it
# in (plex.clustarr.tokenSecret), read inside the shell and never printed.
# The Plex image has no shell, so nothing is run inside it.
set -euo pipefail

CTX=${CTX:-kind-cluster-plex}
NS=${NS:-media}
LEASE=${LEASE:-cluster-plex-plextv}
KIND_NAME=${KIND_NAME:-cluster-plex}
PORT=${PORT:-32499}
PROBE_PORT=${PROBE_PORT:-35004}
TOKEN_SECRET=${TOKEN_SECRET:-clustarr-system/plex-token}
SIZES=${SIZES:-"100 480 481 1000 2000 5000"}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=$ROOT/pkg/plex/api/testdata/livetv
IMAGE=clusterplex-hdhrprobe:dev

k() { kubectl --context "$CTX" -n "$NS" "$@"; }

TMP=$(mktemp -d)
PF='' PROBE_PF=''
cleanup() {
	[ -n "$PF" ] && kill "$PF" 2>/dev/null || true
	[ -n "$PROBE_PF" ] && kill "$PROBE_PF" 2>/dev/null || true
	k delete -f "$ROOT/hack/hdhrprobe/probe.yaml" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	rm -rf "$TMP"
}
trap cleanup EXIT

echo "== the probe"
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$TMP/hdhrprobe" ./hack/hdhrprobe)
cp "$ROOT/hack/hdhrprobe/Dockerfile" "$TMP/"
docker build -q -t "$IMAGE" "$TMP" >/dev/null
kind load docker-image "$IMAGE" --name "$KIND_NAME" >/dev/null
k apply -f "$ROOT/hack/hdhrprobe/probe.yaml" >/dev/null
k rollout status deploy/hdhrprobe --timeout=120s >/dev/null
# PMS reaches the probe at its ClusterIP; the script sets its channel count
# through a port-forward of its own.
PROBE=http://$(k get svc hdhrprobe -o jsonpath='{.spec.clusterIP}')
kubectl --context "$CTX" -n "$NS" port-forward svc/hdhrprobe "$PROBE_PORT:80" >/dev/null 2>&1 &
PROBE_PF=$!
channels() { curl -s -m 10 "http://127.0.0.1:$PROBE_PORT/channels?n=$1" >/dev/null; }

LEADER=$(k get lease "$LEASE" -o jsonpath='{.spec.holderIdentity}')
echo "== PMS on $LEADER"
TOKEN=$(kubectl --context "$CTX" -n "${TOKEN_SECRET%%/*}" get secret "${TOKEN_SECRET#*/}" -o jsonpath='{.data.token}' | base64 -d)
[ -n "$TOKEN" ] || { echo "no token in $TOKEN_SECRET" >&2; exit 1; }
# curl reads the token header from a file in the private temp dir, so the
# token never appears on a command line or in a process listing.
(umask 077 && printf 'X-Plex-Token: %s\n' "$TOKEN" > "$TMP/auth")
kubectl --context "$CTX" -n "$NS" port-forward "pod/$LEADER" "$PORT:32400" >/dev/null 2>&1 &
PF=$!
P=http://127.0.0.1:$PORT
for _ in $(seq 1 30); do curl -s -o /dev/null -m 2 "$P/identity" && curl -s -o /dev/null -m 2 "http://127.0.0.1:$PROBE_PORT/discover.json" && break; sleep 1; done

# call METHOD PATH [NAME]: one PMS call; its answer is written scrubbed to
# NAME.json when NAME is given and left in $TMP/last.json; the status code is
# in $CODE and the path-and-query length in $LEN.
CODE='' LEN=''
call() {
	local m=$1 p=$2 name=${3:-}
	LEN=${#p}
	# The URL goes through a config file: a full channel map is longer than
	# one argument may be.
	printf 'url = "%s%s"\n' "$P" "$p" > "$TMP/url.cfg"
	CODE=$(curl -s -g -m 300 -X "$m" -H 'Accept: application/json' -H @"$TMP/auth" -K "$TMP/url.cfg" \
		-o "$TMP/raw" -w '%{http_code}') || CODE=000
	TOKEN=$TOKEN python3 -I -c 'import os, sys; sys.stdout.write(open(sys.argv[1]).read().replace(os.environ["TOKEN"], "TOKEN"))' \
		"$TMP/raw" > "$TMP/last.json"
	[ -n "$name" ] && cp "$TMP/last.json" "$OUT/$name.json"
	printf '%-6s %-48s -> %s (%s bytes, url %s)\n' "$m" "${p%%\?*}" "$CODE" "$(wc -c < "$TMP/last.json")" "$LEN"
}
q() { python3 -I -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"; }

mkdir -p "$OUT"
UUID=device://tv.plex.grabbers.hdhomerun/CAFE0001
LINEUP="lineup://tv.plex.providers.epg.xmltv/$PROBE/livetv/probe/probe/xmltv.xml#probe"

echo "== the calls"
call GET "/media/grabbers/devices" 01-devices
call POST "/media/grabbers/devices/discover?uri=$(q "$PROBE")" 02-discover
call POST "/media/grabbers/devices?uri=$(q "$PROBE")" 03-add-device
call GET "/media/grabbers/devices" 04-devices
DEVICE=$(jq -r --arg u "$UUID" '.MediaContainer.Device[] | select(.uuid == $u) | .key' "$TMP/last.json")
call POST "/livetv/dvrs?language=eng&device=$(q "$UUID")&lineup=$(q "$LINEUP")" 05-create-dvr
[ "$CODE" = 200 ] || { echo "DVR creation answered $CODE" >&2; exit 1; }
DVR=$(jq -r '.MediaContainer.Dvr[0].key' "$TMP/last.json")
call GET "/livetv/dvrs" 06-dvrs

# mapped: the channel map's entries, and those with a lineup channel.
mapped() { jq -r '[.MediaContainer.ChannelMapping // [] | length, (map(select((.lineupIdentifier // "") != "")) | length)] | @tsv' "$TMP/last.json"; }
# save: the channel map PUT built as SaveChannelMap builds it (url.Values,
# sorted and escaped), from the map in $TMP/last.json.
save() {
	local qs
	qs=$(python3 -I -c '
import json, sys, urllib.parse
m = json.load(open(sys.argv[1]))["MediaContainer"].get("ChannelMapping") or []
q = {"channelsEnabled": ",".join(c["deviceIdentifier"] for c in m)}
for c in m:
    q["channelMappingByKey[%s]" % c["deviceIdentifier"]] = c["channelKey"]
    q["channelMapping[%s]" % c["deviceIdentifier"]] = c["lineupIdentifier"]
print(urllib.parse.urlencode(sorted(q.items())))' "$TMP/last.json")
	call PUT "/media/grabbers/devices/$DEVICE/channelmap?$qs" "${1:-}"
}
# enabled: the device's enabled channels, as PMS lists them.
enabled() { jq -r '[.. | objects | select(has("enabled")) | select(.enabled == "1" or .enabled == true)] | length' "$TMP/last.json"; }

call GET "/livetv/epg/channelmap?device=$(q "$UUID")&lineup=$(q "$LINEUP")" 07-channelmap
save 08-save-channelmap
call GET "/livetv/dvrs/$DVR" 09-dvr
call GET "/media/grabbers/devices/$DEVICE" 09-device
call POST "/livetv/dvrs/$DVR/reloadGuide" 10-reload-guide

echo "== the size test"
for n in $SIZES; do
	channels "$n"
	call POST "/livetv/dvrs/$DVR/reloadGuide"
	# PMS fetches the guide in the background: wait for the map to settle.
	prev='' rescanned=no
	for i in $(seq 1 60); do
		call GET "/livetv/epg/channelmap?device=$(q "$UUID")&lineup=$(q "$LINEUP")" >/dev/null
		cur=$(mapped)
		[ "${cur%%	*}" = "$n" ] && break
		if [ "$i" = 20 ] && [ "$rescanned" = no ]; then
			call POST "/media/grabbers/devices/$DEVICE/scan" "size-$n-scan"
			rescanned=yes
		fi
		[ "$cur" = "$prev" ] && [ "$i" -gt 40 ] && break
		prev=$cur
		sleep 3
	done
	cp "$TMP/last.json" "$OUT/size-$n-channelmap.json"
	map=$(mapped | tr '\t' ,)
	save >/dev/null
	put_code=$CODE put_len=$LEN
	call GET "/media/grabbers/devices/$DEVICE" "size-$n-device" >/dev/null
	echo "size n=$n map(total,lineup)=$map rescanned=$rescanned put=$put_code url=$put_len enabled=$(enabled)"
done

echo "== cleanup"
call DELETE "/livetv/dvrs/$DVR" 11-delete-dvr
call DELETE "/media/grabbers/devices/$DEVICE" 12-delete-device
call GET "/media/grabbers/devices" 13-devices-after
call GET "/livetv/dvrs" 14-dvrs-after
