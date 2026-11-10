#!/bin/sh
# mem.sh: each Plex pod's manager (the container's init) peak and current
# RSS, and its container's cgroup peak and current, read on the kind node:
#
#   docker exec -i cluster-plex-control-plane sh -s < hack/guiderig/mem.sh
#
# The manager's RSS is the number for sizing an in-process guide build; the
# cgroup's covers PMS and the shim too.
for c in $(crictl ps --name '^plex$' -q); do
  pid=$(crictl inspect -o go-template --template '{{.info.pid}}' "$c")
  n=$(crictl inspect -o go-template --template '{{index .status.labels "io.kubernetes.pod.name"}}' "$c")
  cg=$(find /sys/fs/cgroup -type d -name "cri-containerd-$c.scope" 2>/dev/null | head -1)
  echo "$(date -u +%T) $n mgr[$pid $(tr '\0' ' ' < "/proc/$pid/cmdline" | cut -c1-30)] $(grep -E 'VmHWM|VmRSS' "/proc/$pid/status" | tr -s ' \t' ' ' | tr '\n' ' ') cg_peak=$(( $(cat "$cg/memory.peak")/1048576 ))M cg_cur=$(( $(cat "$cg/memory.current")/1048576 ))M"
done
