#!/bin/bash
# oracle-talos.sh: the oracle on a live cluster's installed built guide,
# read-only there. It dumps the plex schema's DDL, the installed guide's
# tables and the cloud-guide cache through kubectl exec on the CNPG
# primary, restores them into a throwaway local PostgreSQL (docker,
# postgres:15-alpine), and runs hack/guidebuild there at the install's own
# time: every row, column and id of the five guide tables compared
# (docs/guide-rig.md, "The oracle on talos").
#
#   SCHEMA=epg_<dvr uuid> hack/guiderig/oracle-talos.sh
#
# CTX (admin@talos), NS (clustarr-system), CLUSTER (the CNPG Cluster,
# clustarr-plex-db), PORT (55612) and NOW (the build's time, Unix seconds;
# default the guide's earliest created_at) are environment overrides. The
# dumps land under WORK (a temp dir by default) and are reused by a second
# run with the same WORK.
set -euo pipefail
: "${SCHEMA:?the installed guide schema, epg_<uuid>}"
D=$(cd "$(dirname "$0")" && pwd)
W=${WORK:-$(mktemp -d)}; mkdir -p "$W"
S=$SCHEMA
k() { kubectl --context "${CTX:-admin@talos}" -n "${NS:-clustarr-system}" "$@"; }
P=$(k get "cluster.postgresql.cnpg.io/${CLUSTER:-clustarr-plex-db}" -o jsonpath='{.status.currentPrimary}')
pg() { k exec "$P" -c postgres -- "$@"; }
# kubectl exec's stdout can drop bytes on a large stream (a taggings row
# came a field short, 2026-11-10): every dump crosses gzipped, so a loss
# fails gunzip's check rather than passing as data.
gz() { local out=$1; shift; for try in 1 2 3; do pg sh -c "$* | gzip -1" > "$out.gz" && gzip -t "$out.gz" 2>/dev/null && gzip -dc "$out.gz" > "$out" && return 0; echo "retry $try: $out" >&2; done; return 1; }
tables=$(for t in metadata_items media_items media_parts tags taggings library_sections; do printf -- '-t %s.%s ' "$S" "$t"; done)
gz "$W/plex-schema.sql" "pg_dump -d plex -n plex --schema-only -O -x"
gz "$W/inst.sql" "pg_dump -d plex -O -x $tables"
now=${NOW:-$(pg psql -d plex -Atc "select min(created_at) from $S.metadata_items where metadata_type<>15")}
start=$(pg psql -d plex -Atc "select convert_from(body,'UTF8')::json->>'start' from clusterplex.epg_cache where key like 'plan/%' limit 1" | cut -c1-10)
F=$W/fixture; rm -rf "$F"; mkdir -p "$F/grid" "$F/metadata"
cache="select key, encode(body,'base64') from clusterplex.epg_cache where key not like 'plan/%'"
gz "$W/cache.txt" "psql -d plex -Atc \"$cache\""
python3 -I "$D/cache2fixture.py" "$F" < "$W/cache.txt"
[ -s "$F/lineup-channels.json" ] || { echo "the cache holds no lineup: fetch it with refetch.py into $F first" >&2; exit 1; }
d2=$(date -u -d "$start + 7 days" +%F)
find "$F/grid" -type f ! -name "*-$start.json" ! -name "*-$d2.json" -delete
docker rm -f guiderig-pg >/dev/null 2>&1 || true
PW=$(head -c 12 /dev/urandom | base64 | tr -dc a-zA-Z0-9)
docker run -d --name guiderig-pg -e POSTGRES_USER=plex -e POSTGRES_PASSWORD="$PW" -e POSTGRES_DB=plex -p "127.0.0.1:${PORT:-55612}:5432" postgres:15-alpine >/dev/null
until docker exec guiderig-pg pg_isready -U plex -q; do sleep 1; done
sleep 2
L() { docker exec -i guiderig-pg psql -q -U plex -d plex -v ON_ERROR_STOP=1 "$@"; }
# The schema's collations and functions need the two extensions in schema
# plex before the DDL, and the DDL before the data.
L -c 'create schema plex; create extension pg_trgm with schema plex; create extension unaccent with schema plex;'
grep -v '^CREATE SCHEMA plex;' "$W/plex-schema.sql" | L >/dev/null
L -c "create schema $S;"
L < "$W/inst.sql" >/dev/null
L -c "alter schema $S rename to epg_inst;"
echo "now=$now start=$start grids=$(find "$F/grid" -type f | wc -l) metadata=$(find "$F/metadata" -type f | wc -l)"
cd "$D/../.."
go run ./hack/guidebuild -dsn "postgres://plex:$PW@127.0.0.1:${PORT:-55612}/plex?sslmode=disable" -fixture "$F" -ref epg_inst -out epg_inst_check -now "$now"
