#!/bin/sh
# guidesum.sh <schema>: a row count and an md5 of every row, in id order, of
# each of a guide schema's five tables, through rigsql.sh. Two guides that
# sum alike are the same guide; a PMS refresh that changes nothing leaves
# the sums alone (docs/guide-rig.md, "Checks").
S=$1
D=$(dirname "$0")
for t in metadata_items media_items media_parts tags taggings; do
  printf '%s %s\n' "$t" "$("$D/rigsql.sh" "select count(*)||' '||md5(coalesce(string_agg(x::text, E'\n' order by x.id),'')) from $S.$t x")"
done
