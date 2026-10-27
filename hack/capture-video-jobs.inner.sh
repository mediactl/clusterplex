# capture-video-jobs.inner.sh runs inside a debug container on plex-0 (see
# capture-video-jobs.sh): sh -s -- <seconds> <mode>. It prints JOB, STATE,
# API, HTTP and PMSLOG lines, all through redact. It never reads a token
# into output: a transcoder's environment is read for EAE_ROOT and
# FFMPEG_EXTERNAL_LIBS alone, and the API token stays in a variable.
#
# Each producer writes its own file, printed one after another at the end:
# concurrent writers to one pipe splice each other's lines. Times are
# seconds since boot (/proc/uptime; busybox date has no %N), and the CLOCK
# line maps them onto the epoch.
secs=$1
mode=$2
trace=${3:-trace} # trace (sockets and files), light (sockets only) or notrace
D=/tmp/capture
mkdir -p "$D"

now() { cut -d' ' -f1 /proc/uptime; }

redact() {
	sed -E \
		-e 's/([Xx]-[Pp][Ll][Ee][Xx]-[Tt][Oo][Kk][Ee][Nn]["=: ]+)[^&[:space:]"]+/\1REDACTED/g' \
		-e 's/([Tt][Oo][Kk][Ee][Nn]["=: ]+)[A-Za-z0-9_-]{8,}/\1REDACTED/g' \
		-e 's/(PLEX_PG_[A-Z_]*=)[^[:space:]]*/\1REDACTED/g'
}

pms_pid() {
	for p in $(pgrep -f 'Plex Media Server$'); do
		[ "$(tr '\0' ' ' <"/proc/$p/cmdline")" = "/usr/lib/plexmediaserver/Plex Media Server " ] && echo "$p" && return
	done
}

# job prints a transcoder's first sighting as one JSON line.
job() {
	p=$1 role=$2
	args=$(tr '\0' '\n' <"/proc/$p/cmdline" | tail -n +2 | jq -R . | jq -cs .)
	eae=$(tr '\0' '\n' <"/proc/$p/environ" | sed -n 's/^EAE_ROOT=//p')
	ffm=$(tr '\0' '\n' <"/proc/$p/environ" | sed -n 's/^FFMPEG_EXTERNAL_LIBS=//p')
	env=$(jq -cn --arg eae "$eae" --arg ffm "$ffm" \
		'{X_PLEX_TOKEN: "REDACTED"} + (if $eae != "" then {EAE_ROOT: $eae} else {} end) + (if $ffm != "" then {FFMPEG_EXTERNAL_LIBS: $ffm} else {} end)')
	pp=$(sed -E 's/^.*\) . ([0-9]+) .*/\1/' "/proc/$p/stat")
	echo "JOB $(jq -cn --arg t "$(now)" --arg pid "$p" --arg ppid "$pp" --arg role "$role" \
		--argjson pms_child "$([ "$pp" = "$PMS" ] && echo true || echo false)" \
		--argjson args "$args" --argjson env "$env" --arg cwd "$(readlink "/proc/$p/cwd")" \
		'{t: $t, pid: $pid, ppid: $ppid, role: $role, pms_child: $pms_child, args: $args, env: $env, cwd: $cwd}')"
}

# watch samples every transcoder twice a second: its first sighting, then
# each change of its process state (R, S, T, ...).
watch() {
	end=$(($(date +%s) + secs))
	seen=" "
	while [ "$(date +%s)" -lt "$end" ]; do
		for d in /proc/[0-9]*; do
			p=${d#/proc/}
			a0=$(tr '\0' '\n' <"$d/cmdline" 2>/dev/null | head -1)
			case "$a0" in
			*"Plex Transcoder") role=shim ;;
			*"Plex Transcoder.real") role=real ;;
			*) continue ;;
			esac
			s=$(sed -E 's/^.*\) (.) .*/\1/' "$d/stat" 2>/dev/null) || continue
			case "$seen" in
			*" $p:$s "*) ;;
			*" $p:"*)
				echo "STATE $p $role $s $(now)"
				seen=$(echo "$seen" | sed -E "s/ $p:. / $p:$s /")
				;;
			*)
				job "$p" "$role"
				echo "STATE $p $role $s $(now)"
				seen="$seen$p:$s "
				[ "$role" = real ] && [ "$trace" != notrace ] && trace "$p"
				;;
			esac
		done
		files
		sleep 0.5
	done
}

# files prints, every 5 s, each session directory's segment count and its
# newest segment: the pace a transcoder writes at, sloth mode included.
files() {
	now=$(cut -d. -f1 /proc/uptime)
	[ $((now % 5)) -eq 0 ] && [ "$now" != "${lastfiles:-}" ] || return 0
	lastfiles=$now
	for d in /proc/[0-9]*; do
		case "$(tr '\0' '\n' <"$d/cmdline" 2>/dev/null | head -1)" in
		*"Plex Transcoder.real") ;;
		*) continue ;;
		esac
		# The cwd link resolves in the transcoder's own mount namespace.
		segs=$(ls "$d/cwd/" 2>/dev/null | grep '^media-.*\.ts$')
		echo "FILES ${d#/proc/} $(echo "$segs" | grep -c .) $(echo "$segs" | sort | tail -1) $(now)"
	done
}

# trace records a real transcoder's socket calls: its requests to PMS (the
# segment list, progress, a live input) with their bodies, PMS's answers,
# and how long each call blocked (-T), which is how a throttle shows. Plex's
# transcoder writes and reads its sockets as files, so every read and write
# is traced and sockets() keeps those on a connection to port 32400.
# A light trace (light) records only socket calls: the progress PUTs and
# PMS's answers, without the media reads whose tracing slows a transcoder.
trace() {
	calls=%network,read,readv,write,writev,close
	[ "$trace" = light ] && calls=%network
	timeout "$secs" strace -f -tt -T -s 2048 -e trace=$calls \
		-p "$1" -o "$D/strace.$1" 2>/dev/null &
}

# sockets keeps a trace's lines on file descriptors connected to port
# 32400, or carrying an HTTP request (one opened before strace attached),
# until their close, across threads' unfinished and resumed halves.
sockets() {
	awk '
		function fdof(line) { if (match(line, /[a-z]+\(([0-9]+)/)) { s = substr(line, RSTART, RLENGTH); sub(/^[a-z]+\(/, "", s); return s } return "" }
		/connect\(/ && /htons\(32400\)/ { fd = fdof($0); open[fd] = 1; print; next }
		/(sendto|write|writev)\([0-9]+, \[?\{?(iov_base=)?"(GET|PUT|POST|DELETE) / { fd = fdof($0); open[fd] = 1; print; next }
		/resumed>/ { if (pending[$1]) { print; pending[$1] = 0 } next }
		/close\(/ { fd = fdof($0); if (open[fd]) { print; delete open[fd] } next }
		{ fd = fdof($0); if (fd != "" && open[fd]) { print; if ($0 ~ /<unfinished/) pending[$1] = 1 } }
	' "$1"
}

# wire prints each packet to or from PMS's port 32400 on plex0, its veth
# (clients, through the proxy), whose payload is a request line, a status
# line, a segment-list line or a throttle word, with its tcpdump header
# (time, ports), so a long request shows as one connection. A transcoder
# reaches PMS on PMS's own loopback, which this container cannot enter:
# trace records those.
wire() {
	timeout "$secs" tcpdump -i plex0 -l -nn -A -s 0 'tcp port 32400' 2>/dev/null | awk '
		/^[0-9][0-9]:[0-9][0-9]:[0-9][0-9]\./ { if (keep) print hdr; for (i = 1; i <= n; i++) if (keep) print "  " body[i]; hdr = $0; n = 0; keep = 0; next }
		{ n++; body[n] = $0 }
		/(GET|POST|PUT|DELETE) \/|HTTP\/1\.[01] [0-9][0-9][0-9]|media-[0-9]+\.ts|[Tt]hrottl|[Ss]loth|Transfer-Encoding|Content-Length/ { keep = 1 }
		END { if (keep) print hdr }
	' | sed 's/^/HTTP /'
}

# api starts a conversion through Plex's API as a client would, and holds
# it: Plex for iOS's HLS by default, Plex Web's DASH for dash-encode. The
# mode picks the item and what is asked of it:
#   library-hls        the newest film, 720p at 3 Mb/s
#   library-hls-seek   the same, restarted 30 min in
#   library-hls-fast   the same at 180p and 300 kb/s, which runs ahead
#   library-hls-play   library-hls-fast, its segments read as a player
#                      reads them: three at once, then one every 8 s.
#                      PMS ends a session nobody reads from after about
#                      3 min, so only this one runs into Plex's throttle.
#   hdr-sdr            Jaws (1975), HDR10, at 1080p 4 Mb/s
#   dash-encode        the newest film, in Plex Web, 720p 3 Mb/s
#   burn-image         Arrietty (2010), its PGS subtitle burned in
#   burn-text-sidecar  127 Hours (2010), its SRT sidecar burned in
# A burn-in mode selects the subtitle for the item as a client does, and
# selects none again afterwards.
api() {
	apk add -q curl >/dev/null 2>&1 || true
	R=/proc/$PMS/root
	T=$(sed -n 's/.*PlexOnlineToken="\([^"]*\)".*/\1/p' "$R/var/lib/plexmediaserver/Library/Application Support/Plex Media Server/Preferences.xml")
	B=http://127.0.0.1:32400
	SID=capture-$$
	product="Plex for iOS" platform=iOS device=iPhone proto=hls ext=m3u8
	res=1280x720 rate=3000 title= burn=
	case "$mode" in
	library-hls-fast | library-hls-play) res=320x180 rate=300 ;;
	hdr-sdr) title="Jaws" res=1920x1080 rate=4000 ;;
	dash-encode) product="Plex Web" platform=Chrome device=Linux proto=dash ext=mpd ;;
	burn-image) title="Arrietty" burn=image ;;
	burn-text-sidecar) title="127 Hours" burn=sidecar ;;
	esac
	set -- -H "X-Plex-Token: $T" -H "X-Plex-Client-Identifier: $SID" -H "X-Plex-Product: $product" \
		-H "X-Plex-Platform: $platform" -H "X-Plex-Device: $device" -H "Accept: application/json"
	if [ -n "$title" ]; then
		key=$(curl -s -G "$@" --data-urlencode "title=$title" "$B/library/sections/1/all?type=1" |
			jq -r '.MediaContainer.Metadata[0].ratingKey // empty')
	else
		key=$(curl -s "$@" "$B/library/sections/1/all?type=1&X-Plex-Container-Start=0&X-Plex-Container-Size=1&sort=addedAt:desc" |
			jq -r '.MediaContainer.Metadata[0].ratingKey // empty')
	fi
	echo "API item $key"
	subs=none part=
	if [ -n "$burn" ]; then
		meta=$(curl -s "$@" "$B/library/metadata/$key")
		part=$(echo "$meta" | jq -r '.MediaContainer.Metadata[0].Media[0].Part[0].id')
		case "$burn" in
		image) pick='select(.streamType == 3 and (.codec == "pgs" or .codec == "vobsub" or .codec == "dvd_subtitle"))' ;;
		sidecar) pick='select(.streamType == 3 and .key != null)' ;;
		esac
		sub=$(echo "$meta" | jq -r "[.MediaContainer.Metadata[0].Media[0].Part[0].Stream[] | $pick][0].id // empty")
		echo "API subtitle $sub ($burn) on part $part"
		curl -s -o /dev/null -w "API select %{http_code}\n" -X PUT "$@" "$B/library/parts/$part?subtitleStreamID=$sub&allParts=1"
		subs=burn
	fi
	q="path=%2Flibrary%2Fmetadata%2F$key&protocol=$proto&directPlay=0&directStream=0&directStreamAudio=1&videoResolution=$res&maxVideoBitrate=$rate&videoQuality=100&session=$SID&mediaIndex=0&partIndex=0&fastSeek=1&location=lan&subtitles=$subs"
	start() {
		curl -s -o /tmp/start.out -w "API start %{http_code}\n" "$@" "$B/video/:/transcode/universal/start.$ext?$q$off"
		if [ "$ext" = m3u8 ]; then
			v=$(grep -v '^#' /tmp/start.out | head -1)
			curl -s -o /tmp/index.m3u8 -w "API index %{http_code}\n" "$@" "$B/video/:/transcode/universal/$v"
		fi
	}
	off=
	start "$@"
	if [ "$mode" = library-hls-seek ]; then
		sleep 30
		curl -s -o /dev/null -w "API stop %{http_code}\n" "$@" "$B/video/:/transcode/universal/stop?session=$SID"
		off="&offset=1800"
		start "$@"
	fi
	if [ "$mode" = library-hls-play ]; then
		play "$@"
	else
		sleep $((secs - 40))
	fi
	curl -s -o /dev/null -w "API stop %{http_code}\n" "$@" "$B/video/:/transcode/universal/stop?session=$SID"
	if [ -n "$part" ]; then
		curl -s -o /dev/null -w "API unselect %{http_code}\n" -X PUT "$@" "$B/library/parts/$part?subtitleStreamID=0&allParts=1"
	fi
}

# play reads the session's segments as a player does, three ahead of a
# clock that runs from the first: segment n when 8(n-3) s have passed. Each
# GET waits until the transcoder has written it.
play() {
	base=$B/video/:/transcode/universal/${v%/*}
	began=$(now)
	n=0
	grep -v '^#' /tmp/index.m3u8 | while read -r seg; do
		el=$(awk -v a="$began" -v b="$(now)" 'BEGIN { print int(b - a) }')
		[ "$el" -ge $((secs - 50)) ] && break
		due=$(( (n - 3) * 8 ))
		[ "$due" -gt "$el" ] && sleep $((due - el))
		curl -s -o /dev/null -w "API segment $n %{http_code} %{size_download} %{time_total} $(now)\n" "$@" "$base/$seg"
		n=$((n + 1))
	done
}

pmslog() {
	f="/proc/$PMS/root/var/lib/plexmediaserver/Library/Application Support/Plex Media Server/Logs/Plex Media Server.log"
	grep -aiE 'throttl|sloth' "$f" | tail -n 200 | sed 's/^/PMSLOG /'
}

main() {
	PMS=$(pms_pid)
	echo "PMS $PMS"
	echo "CLOCK $(date +%s) $(now)"
	wire >"$D/wire" 2>&1 &
	w=$!
	a=
	case "$mode" in
	library-hls | library-hls-seek | library-hls-fast | library-hls-play | hdr-sdr | dash-encode | burn-image | burn-text-sidecar)
		api >"$D/api" 2>&1 &
		a=$!
		;;
	esac
	watch >"$D/watch" 2>&1
	sleep 5
	kill "$w" $a 2>/dev/null
	cat "$D/watch" "$D/api" "$D/wire" 2>/dev/null
	for f in "$D"/strace.*; do
		[ -e "$f" ] && sockets "$f" | sed "s/^/TRACE ${f##*.} /"
	done
	pmslog
}

main 2>&1 | redact

# sh -s reads its script from stdin, which kubectl debug -i leaves open:
# without exit the shell waits there for more, and the container never ends.
exit 0
