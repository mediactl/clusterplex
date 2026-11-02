#!/usr/bin/env bash
# repair-bundle-hash.sh finds the movie and show rows whose
# metadata_items.hash is not the sha1 of their guid, the bundle PMS keeps
# their images in, and with --apply gives each its guid's hash and has
# Plex refresh it into that bundle, one at a time.
#
# A row the projection re-keyed before plexproject.BundleHash kept its old
# hash; its refresh wrote the images to the new guid's bundle, which the
# weekly clean-bundles job then deleted as named by no row (talos,
# 2026-11-01: 72 movies that gained their edition, with no poster or
# background). Run this before the next clean-bundles (Sunday 04:00) or
# the bundles a refresh fills are named by a row and kept.
#
#   hack/repair-bundle-hash.sh            # list what would change
#   hack/repair-bundle-hash.sh --apply    # write each hash, refresh, check
#   ONLY=692 hack/repair-bundle-hash.sh --apply   # those ids alone (a,b,c)
#
# KUBE_CONTEXT, NAMESPACE, DB_CLUSTER and PLEX_URL name the install; the
# token is read from the plex-token Secret and never printed.
set -euo pipefail

KUBE_CONTEXT=${KUBE_CONTEXT:-admin@talos}
NAMESPACE=${NAMESPACE:-clustarr-system}
DB_CLUSTER=${DB_CLUSTER:-clustarr-plex-db}
PLEX_URL=${PLEX_URL:-http://10.1.1.102:32400}
REFRESH_WAIT=${REFRESH_WAIT:-180} # seconds to wait for each refresh
ONLY=${ONLY:-}                    # comma-separated row ids, or every row

apply=false
[ "${1:-}" = --apply ] && apply=true

kc() { kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" "$@"; }

primary=$(kc get cluster.postgresql.cnpg.io "$DB_CLUSTER" -o jsonpath='{.status.currentPrimary}')
[ -n "$primary" ] || { echo "no primary for $DB_CLUSTER" >&2; exit 1; }

# sql runs one statement on the primary, fields separated by US (titles
# and paths may hold a tab).
sql() {
	kc exec "$primary" -c postgres -- psql -d plex -v ON_ERROR_STOP=1 -X -q -At -F $'\x1f' \
		-c "SET search_path = plex, public" -c "$1"
}

# Movies and shows keep their images in their own bundle; seasons and
# episodes read theirs from the show's. Live TV's tv.plex.xmltv rows are
# Plex's own and left alone.
plan=$(sql "SELECT id, metadata_type, hash, md5(guid), guid, title FROM metadata_items
	WHERE metadata_type IN (1, 2) AND deleted_at IS NULL AND hash IS NOT NULL
	AND (guid LIKE 'plex://%' OR guid LIKE 'tv.plex.agents.custom.clustarr.%')
	ORDER BY id" | ONLY="$ONLY" python3 -I -c '
import hashlib, os, sys
only = {i for i in os.environ["ONLY"].split(",") if i}
for line in sys.stdin:
    f = line.rstrip("\n").split("\x1f")
    if len(f) != 6 or (only and f[0] not in only):
        continue
    want = hashlib.sha1(f[4].encode()).hexdigest()
    if f[2] != want:
        print("\x1f".join([f[0], f[1], f[2], want, f[3], f[4], f[5]]))
')

if [ -z "$plan" ]; then
	echo "every movie and show row carries its guid's hash"
	exit 0
fi
echo "$plan" | awk -F $'\x1f' '{printf "%s\ttype %s\t%s -> %s\t%s\t%s\n", $1, $2, substr($3,1,8), substr($4,1,8), $6, $7}'
echo "$(echo "$plan" | wc -l) rows"
$apply || { echo "dry run: pass --apply to write and refresh"; exit 0; }

token=$(kc get secret plex-token -o jsonpath='{.data.token}' | base64 -d)
plex() { curl -sS -m 30 -H "X-Plex-Token: $token" "$@"; }

ok=0 failed=0
while IFS=$'\x1f' read -r id _ old want guidmd5 _ title; do
	before=$(sql "SELECT coalesce(refreshed_at, 0) FROM metadata_items WHERE id = $id")
	# Compare-and-swap on the hash and guid read above: a row Plex or the
	# projection changed since is left for the next run.
	n=$(sql "UPDATE metadata_items SET hash = '$want' WHERE id = $id AND hash = '$old' AND md5(guid) = '$guidmd5' RETURNING id" | grep -c . || true)
	if [ "$n" != 1 ]; then
		echo "$id $title: changed since read, skipped"
		failed=$((failed + 1))
		continue
	fi
	plex -X PUT -o /dev/null "$PLEX_URL/library/metadata/$id/refresh"
	waited=0
	until [ "$(sql "SELECT coalesce(refreshed_at, 0) FROM metadata_items WHERE id = $id")" -gt "$before" ]; do
		if [ "$waited" -ge "$REFRESH_WAIT" ]; then
			break
		fi
		sleep 3
		waited=$((waited + 3))
	done
	thumb=$(plex -o /dev/null -w '%{http_code}' "$PLEX_URL/library/metadata/$id/thumb/1" || true)
	art=$(plex -o /dev/null -w '%{http_code}' "$PLEX_URL/library/metadata/$id/art/1" || true)
	echo "$id $title: hash ${want:0:8}, refreshed after ${waited}s, thumb $thumb, art $art"
	case "$thumb $art" in
	"200 200" | "302 200" | "200 302" | "302 302") ok=$((ok + 1)) ;;
	*) failed=$((failed + 1)) ;;
	esac
done <<<"$plan"
echo "$ok repaired, $failed to look at"
