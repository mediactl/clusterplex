#!/bin/sh
# rigsql.sh <sql>: runs SQL on the rig's library database through its
# plex-postgres pod, unaligned, '|' separated, failing on the first error.
# CTX and NS pick the cluster (kind-cluster-plex, media by default).
P=$(kubectl --context "${CTX:-kind-cluster-plex}" -n "${NS:-media}" get pod -l app=plex-postgres -o name | head -1)
kubectl --context "${CTX:-kind-cluster-plex}" -n "${NS:-media}" exec -i "$P" -- psql -U plex -d plex -tA -F'|' -v ON_ERROR_STOP=1 -c "$1"
