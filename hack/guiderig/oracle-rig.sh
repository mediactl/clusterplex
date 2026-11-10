#!/bin/bash
# oracle-rig.sh <tag>: the oracle on the rig's installed built guide. It
# copies the installed guide's five tables to epg_inst_<tag>, exports the
# manager's cloud-guide cache (clusterplex.epg_cache) as a fixture, keeps
# only the plan's two grid weeks, and runs hack/guidebuild on it at the
# install's own time: an independent build, compared with the installed
# guide in every row, column and id (docs/guide-rig.md, "The oracle").
#
#   SCHEMA=epg_<dvr uuid> GUIDEBUILD_DSN=postgres://plex:...@127.0.0.1:55499/plex hack/guiderig/oracle-rig.sh t1
#
# SCHEMA is the installed guide's schema (select schema_name from
# information_schema.schemata where schema_name like 'epg\_%'); the DSN
# reaches the rig's plex-postgres, port-forwarded. CTX and NS are rigsql.sh's.
set -euo pipefail
: "${SCHEMA:?the installed guide schema, epg_<uuid>}"
: "${GUIDEBUILD_DSN:?a DSN to the rig library database}"
D=$(cd "$(dirname "$0")" && pwd); tag=$1; S=$SCHEMA
R=$D/rigsql.sh
W=${WORK:-$(mktemp -d)}
now=$($R "select min(created_at) from $S.metadata_items where metadata_type<>15")
$R "DROP SCHEMA IF EXISTS epg_inst_$tag CASCADE; CREATE SCHEMA epg_inst_$tag; $(for t in metadata_items media_items media_parts tags taggings library_sections; do printf 'CREATE TABLE epg_inst_%s.%s AS TABLE %s.%s; ' "$tag" "$t" "$S" "$t"; done)" >/dev/null
F=$W/fixture-$tag; rm -rf "$F"; mkdir -p "$F/grid" "$F/metadata"
$R "select key, encode(body,'base64') from clusterplex.epg_cache where key not like 'plan/%'" | python3 -I "$D/cache2fixture.py" "$F"
# Only the plan's grids: the cache may hold an older day's.
start=$($R "select convert_from(body,'UTF8')::json->>'start' from clusterplex.epg_cache where key like 'plan/%' limit 1" | cut -c1-10)
d2=$(date -u -d "$start + 7 days" +%F)
find "$F/grid" -type f ! -name "*-$start.json" ! -name "*-$d2.json" -delete
echo "now=$now start=$start grids=$(find "$F/grid" -type f | wc -l) metadata=$(find "$F/metadata" -type f | wc -l)"
cd "$D/../.."
go run ./hack/guidebuild -fixture "$F" -ref "epg_inst_$tag" -out "epg_inst_${tag}_check" -now "$now"
