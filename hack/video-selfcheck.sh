#!/bin/sh
# video-selfcheck.sh proves a cluster-plex-video image: its worker loads
# FFmpeg 9 through a shim of API 2 or later, and finds the encoders,
# filters and Intel media driver the GPU pool converts with. It needs no
# GPU: the self-check measures nothing.
#
#   hack/video-selfcheck.sh <image>
set -eu
image=${1:?usage: video-selfcheck.sh <image>}
out=$(docker run --rm --entrypoint /usr/bin/video-worker "$image" --self-check --encoder libx264 2>&1) || {
	echo "$out" >&2
	echo "video-selfcheck: the self-check failed" >&2
	exit 1
}
echo "$out"
fail=0
want() {
	echo "$out" | grep -qx "$1" || {
		echo "video-selfcheck: missing line: $1" >&2
		fail=1
	}
}
want "libavcodec 63"
api=$(echo "$out" | sed -n 's/^shim api //p')
[ "${api:-0}" -ge 2 ] || {
	echo "video-selfcheck: shim API ${api:-none}, want 2 or later" >&2
	fail=1
}
for e in h264_qsv libx264 aac; do want "encoder $e: found"; done
for f in vpp_qsv hwmap hwupload subtitles bwdif zscale tonemap scale; do want "filter $f: found"; done
want "driver /usr/lib/x86_64-linux-gnu/dri/iHD_drv_video.so: present"
exit $fail
