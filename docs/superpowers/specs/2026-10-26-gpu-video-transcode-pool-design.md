# GPU video-transcode pool: Plex's video conversions on the Intel NUC

**Status:** approved by the owner (2026-10-26); amended the same day (owner): the worker writes nothing to disk
**Repositories:** cluster-plex (the pool, the route, the chart) and clustarr
(the transcode admission's yield to live streams)

## Why

On 2026-10-26 Live TV on Plex for iOS failed with "This item can't be played
right now".

- **What the app asks for:** a video transcode for every Live TV tune
  (`/video/:/transcode/universal/decision?directPlay=0&directStream=0`). It
  does so even on the LAN at original quality, and for channels that are
  plain progressive H.264 High 720p with AAC.
- **Why Plex refused it:** cluster-plex had set `TranscoderCanOnlyRemuxVideo=1`
  since 2026-10-01, so every such request got decision 4010.
- **The interim fix:** the owner allowed video conversion (cluster-plex
  `f73c548`, `TranscoderCanOnlyRemuxVideo=0`).
- **Why that isn't enough:** every conversion then ran as a software libx264
  encode on plex-0's node, talos-10-1-40-33. That's a 4-core arm64
  Raspberry Pi and one of the three etcd members. The owner's verdict:
  "terrible".

The owner chose to move Plex's video conversions onto the Intel GPU in the
NUC (talos-88-ae-dd-75-3d-a0). It's done on the pattern cluster-plex already
uses for Plex Web's DASH remuxes: the manager classifies a `Plex Transcoder`
job, a worker pool runs it in process on ffgo, and the relay streams the
output back into Plex's session directory. Plex stays where it is.

## What the owner decided

These are the owner's answers (2026-10-26). Build to them as written.

1. **Scope:** every video conversion Plex is asked for: Live TV and library
   playback, from any client.
   - This includes the conversions outside playback (owner, 2026-10-26,
     after the review): downloads such as Plex for iOS sync, Optimize
     versions, and the DVR's "convert video while recording".
   - These write a whole file rather than stream segments. See §3's
     "Whole-file jobs".
2. **Priority:** a viewer's stream takes priority over clustarr's batch
   encodes on the shared GPU. Approved mechanism:
   - while any stream is live, clustarr's transcode admission admits no new
     file to its Intel class;
   - an encode already running is not cut short.
3. **No fallback:** when the pool cannot take a stream (the NUC down, the
   pool full, a shape the pool does not understand), Plex refuses it and the
   client shows its error. Plex never converts video on its own node again.
4. **Capacity:** at most 3 converted streams at once, matching the IPTV
   account's three connections.
5. **Start-up:** the pool adds at most about 2 s beyond what the first
   segment's input itself takes to arrive. This target is revised after the
   review (owner, 2026-10-26).
   - **Why not "picture within 10 s":** that may be impossible for any
     transcoder. Live TV arrives in real time, and Plex asks for segments of
     `segment_time` seconds (8 in the captured library job), so the first
     segment cannot be finished before that much of the channel has arrived.
     That comes on top of the ~7 s tune (the tune POST took 6.9 s on
     2026-10-26).
   - **How it's measured:** per job, as the time from the job's start to the
     first segment's file, minus the input time the segment covers. No
     separate baseline is needed.
6. **Burn-in:** subtitle burn-in is in the first version. When Plex asks for
   subtitles burned into the picture, the pool does it.
7. **Placement:** Plex stays on its control-plane node, next to its database
   and on the node the load balancer's routing depends on. Only the
   conversion moves.

Also settled:

- `TranscoderCanOnlyRemuxVideo` stays `0`. Plex only spawns a video
  conversion when it is allowed to, and the pool is what serves it.
- Streams that only remux, or convert audio alone, are left as today. They
  run on Plex's node, or on the remux pool for Plex Web's DASH.

## Goals

- **Live TV on Plex for iOS** plays smoothly at the resolution and bitrate
  Plex asked for. "Smoothly" means segments are produced faster than real
  time with no rebuffering, the start-up target above is met, and 3 streams
  run at once.
- **Library playback** that Plex decides to convert plays the same way,
  whatever the client.
- **No video encode or decode for a conversion** ever runs on a Plex pod.
- **Batch work yields:** while streams are live, clustarr's batch re-encodes
  admit no new file to the Intel class.
- **Clients can't tell:** Plex and its clients see exactly what Plex's own
  transcoder would have produced, in segment names, manifests and progress.

## Non-goals

- **Making clients direct-stream.** Plex for iOS's choice to ask for a
  transcode is not changed.
- **Moving Plex onto the NUC,** or using Plex's own hardware transcoding.
- **HEVC or AV1 output.** The pool encodes H.264, which is what Plex asks
  for in every captured job. Another target is a refusal.
- **NVIDIA.** talos has no NVIDIA node.
- **Plex's relay through plex.tv.** `RelayEnabled` stays `0`.
- **Audio-only conversions.** Remuxes stay where they are.
- **More than one GPU node.** The design must not preclude it, but it is
  built for the NUC.

## Anti-goals: a failure even if it works

- **A silent fallback.** A video conversion that quietly falls back to
  running on Plex's node, the Pi.
- **Interrupting clustarr.** An encode that clustarr has already started
  being stopped or restarted because a stream started.
- **A visible difference.** Any change Plex or a client can see in segment
  file names, manifest lines, progress callbacks or exit behaviour compared
  with Plex's own transcoder. If the app behaves differently from a stock
  Plex conversion, it's wrong.
- **Hidden decisions.** A route that decides by guesswork. A job the pool
  doesn't understand is refused and logged with its reason; it is never
  "tried anyway".
- **Breaking clustarr's invariants.** clustarr's manager stays the only
  writer of clustarr's resources, and agents never write Kubernetes.

## What exists, and the gaps

### The remux pool

The remux pool is cluster-plex's existing worker pattern. See ADR-0007 and
`docs/superpowers/specs/2026-10-01-browser-remux-transcoder-design.md`.

**The route.**
- `Plex Transcoder` is a shim (`cmd/shim`) that sends each job to the
  manager's exec server (`cmd/manager/execserver.go`).
- `remoteexec.Dispatcher.Execute` tries these routes in order: scan, then
  `Remux` (`pkg/remux/relay`), then `tryRemote` to another Plex pod, then
  `Local`.

**The relay** (`pkg/remux/relay/relay.go`):
- picks a ready worker by hash ring;
- streams `File`, `Manifest`, `Progress` and `Done` events (`proto/remuxpb`);
- writes each file into the shim's `cwd` (Plex's session directory) as
  `.<name>.tmp` and renames it;
- only after that tells Plex: it POSTs the manifest and forwards progress
  PUTs, with the session token, to Plex's own address;
- maps a worker error to exit code 1 with `remux: <err>` on stderr.

**The pieces that are DASH-only:**
- the parser's allow-list (`pkg/remux/parse.go`), which needs `-f dash`,
  copied video and exactly two maps;
- `segmentName` in the relay;
- the MPD renderer;
- the proto.

**What's missing for this work:**
- The pool's pods have no GPU, no `/dev/dri` and no Intel runtime: the
  remux image is FROM scratch with BtbN's FFmpeg 9 `gpl-shared`.
- Nothing limits how many jobs one pod takes.
- `/readyz` always answers ok.
- The remux pool is off on talos (`remux-selector: ""`).

### clustarr's Intel stack

clustarr's QSV tier (`pkg/transcode/engine`, `app/transcode/worker/inprocess`,
`images/Dockerfile.native`) already does most of this on ffgo, on the same
NUC:

- **Decode:** on the GPU through VA-API on the Intel render node. The pod's
  start-up trial decides which formats qualify; on the NUC it measured
  `h264:8, hevc:8, hevc:10, mpeg2video:8, vp9:8, vp9:10`.
- **Encode:** frames are mapped to QSV (`hwmap=derive_device=qsv,format=qsv`),
  converted with `vpp_qsv`, and encoded with `hevc_qsv`. Any other format is
  decoded on the CPU and uploaded.
- **Image:** the native image stages the Intel runtime:
  `intel-media-va-driver-non-free`, `libmfx-gen1.2`, `libva2` (2.22),
  `libva-drm2`, `libmfx1`, with `LIBVA_DRIVER_NAME=iHD`.

**What's missing:**
- It never uses `h264_qsv`, never scales, never burns subtitles in, and
  never writes HLS or DASH.
- Nothing yields the GPU to another user.

**Capacity:**
- The NUC advertises `gpu.intel.com/i915: 4` on talos.
- talos runs clustarr's Intel class at `slots intel=2` (the chart's default
  is 1), so up to two clustarr encodes may be running when a stream starts,
  and they finish (owner's rule).
- One clustarr QSV encode runs at roughly 20× real time (`hack/presetbench`,
  commit 10e1afdb).
- `intel=2` stays (my call). The talos verification measures the worst
  case: three streams beside two running clustarr encodes. A stream falling
  below 1× there goes back to the owner before any setting changes.

### Plex's request

This is the job a video conversion has to honour. It was captured on talos
on 2026-10-26: a library film, asked for as Plex for iOS would, HLS, 720p,
3 Mb/s.

```
Plex Transcoder -codec:0 hevc -codec:2 eac3_eae -eae_prefix:2 <session>_
  -analyzeduration 20000000 -probesize 20000000
  -i "/media/movies/Grizzly Man (2005) {tmdb-501}/… .mp4"
  -start_at_zero -copyts -fps_mode cfr -y -nostats -loglevel quiet -loglevel_plex error
  -progressurl http://127.0.0.1:32400/video/:/transcode/session/<session>/<uuid>/progress
  -filter_complex "[0:0]scale=w=720:h=404:force_divisible_by=4[0];[0]format=pix_fmts=yuv420p|nv12[1]"
  -map [1] -codec:0 libx264 -crf:0 17 -maxrate:0 1818k -bufsize:0 3636k -r:0 23.975999999999999
  -preset:0 veryfast -x264opts:0 subme=2:me_range=4:rc_lookahead=10:me=dia:no_chroma_me:8x8dct=0:partitions=none
  -force_key_frames:0 expr:gte(t,n_forced*8)
  -filter_complex "[0:2] aresample=async=1:ochl='stereo':rematrix_maxval=0.000000dB:osr=96000[2]"
  -map [2] -metadata:s:1 language=eng -codec:1 aac -b:1 232k
  -segment_format mpegts -f ssegment -individual_header_trailer 0 -segment_time 8
  -segment_start_number 0 -segment_time_delta 0.0625
  -segment_list http://127.0.0.1:32400/video/:/transcode/session/<session>/<uuid>/manifest?X-Plex-Http-Pipeline=infinite
  -segment_list_type csv -segment_list_size 5 -segment_list_separate_stream_times 1
  -segment_list_unfinished 1 -max_delay 5000000 -avoid_negative_ts disabled
  -map_metadata -1 -map_chapters -1 media-%05d.ts
env: EAE_ROOT=/tmp/pms-…/EasyAudioEncoder, FFMPEG_EXTERNAL_LIBS=…/Codecs/…-linux-aarch64/, X_PLEX_TOKEN=…
cwd: /transcode/Transcode/Sessions/plex-transcode-<session>-<uuid>
```

What this job shows:

- **The output contract:** MPEG-TS segments `media-%05d.ts`, each starting
  on a keyframe forced every `segment_time` seconds. Each finished segment is
  reported as a CSV line to the `-segment_list` URL.
- **The decoder hints:** `-codec:N` before `-i` is a decoder hint, and
  `eac3_eae` names Plex's EasyAudioEncoder. The pool decodes E-AC-3 with
  FFmpeg's own decoder and needs no EAE.
- **The audio:** converted to stereo AAC on the CPU; that is cheap.
- **What Plex asks for:** the video encode's target is given as x264
  settings (`-crf`, `-maxrate`, `-bufsize`, `-r`, `-preset`, `-x264opts`).
  The pool maps the rate limits and frame rate onto `h264_qsv`. The x264
  tuning (`preset`, `x264opts`) has no QSV meaning and is ignored.

**Still to capture:** a Live TV job, a burn-in job and a DASH-with-encode
job have not been captured yet. Capturing them is the build's first step.

- Live TV's input is Plex's own live session rather than a file; the
  2026-10-26 debug log showed the recorder reading
  `http://169.254.47.1:47000/stream/<ch>` and the client's transcode reading
  `/livetv/sessions/<id>`.
- How to capture a job: start the transcode through Plex's API, read the
  running `Plex Transcoder`'s `/proc/<pid>/cmdline`, `environ` and `cwd`
  from a debug container, then stop the session.
- **Redact** `X_PLEX_TOKEN` and every `PLEX_PG_*` value before saving a
  fixture.

## Design

The parts below are my calls unless marked as the owner's. A builder may
change a call if the code argues for it, but should say so.

### 1. The route: every video conversion goes to the pool, or fails

- **A new dispatcher route, `Video`,** runs right after `Remux`, before
  `tryRemote` and `Local`.
- **What counts as a video conversion:** a `Plex Transcoder` job is one when
  any output stream mapped from a video input has a codec other than `copy`.
  It's judged from the argv's maps and `-codec:N` values, not from the
  session.
- **Such a job never reaches `tryRemote` or `Local`** (owner: no fallback).
  - If the pool takes it, the relay serves it.
  - If the pool can't (no ready pod with a free slot, a dial failure, a
    shape the parser refuses, or the worker failing before its first
    segment), the shim exits non-zero with `video: <reason>` on stderr.
    Plex then fails the session as it would any failed transcode.
- **Everything else is untouched:** remuxes, audio-only conversions, scans
  and the Remux route behave as today.
- **Metrics:** each route and refusal is counted, with a bounded `reason`
  label: `no-pool`, `full`, `shape`, `worker-error`, `cancelled`.

### 2. The parser: Plex's shapes, as an allow-list

The parser follows `pkg/remux/parse.go`: an allow-list built from recorded
argvs, where an unknown option is a refusal (`shape`).

**What it reads:**
- the input, `-ss`, `-start_at_zero` and `-copyts`;
- each `-filter_complex` chain, with its scale, format, fps and subtitle
  burn-in parts;
- each `-map`;
- per output, the codec, `-crf`, `-maxrate`, `-bufsize`, `-r`,
  `-force_key_frames expr:gte(t,n_forced*N)` and the audio bitrate;
- the muxer's `-f ssegment` and `-segment_format mpegts`, or `-f dash` with
  an encoded video;
- `-segment_time`, `-segment_start_number`, `-segment_list` with its type
  and size, the output pattern, `-progressurl` and `-manifest_name`;
- from the environment, `X_PLEX_TOKEN`.

**Tone mapping and deinterlacing are in scope.** They follow from the owner's
"all video conversion" (settled after the review):
- HDR10 library films converted to SDR H.264, for players without HDR or
  HEVC support;
- interlaced Live TV channels (1080i, MPEG-2).

Whatever filters Plex's argv asks for to do this are parsed and run on the
GPU, through `vpp_qsv` or the VA-API filters as ffgo's filter graph gives
them. The start-up check measures them. An HDR job and an interlaced Live
TV job join the capture list.

**What it accepts and ignores:** the decoder hints `-codec:N` before `-i`,
`-eae_prefix`, `-analyzeduration` and `-probesize`, the log options, and
x264's `-preset` and `-x264opts`.

**Fixtures:** every recorded argv becomes a test fixture under
`pkg/…/testdata`, as `plex-dash-jobs.log` is for the remux pool, with
secrets redacted.

### 3. The worker: a GPU pool on the NUC

A new binary and a new Deployment, separate from the remux pool. The image
needs the Intel runtime and is amd64-only, while the remux image stays
small and multi-arch.

**The image:**
- cluster-plex's FFmpeg 9 build (BtbN `gpl-shared`, pinned by SHA-256, as
  `images/Dockerfile.remux` pins it), plus the ffgo shim;
- the Intel runtime as clustarr's `Dockerfile.native` stages it:
  `intel-media-va-driver-non-free`, `libmfx-gen1.2`, `libva2` ≥ 2.21,
  `libva-drm2`, and the `LIBVA_*` environment;
- no `ffmpeg` executable.

**Placement:**
- one share of `gpu.intel.com/i915` per pod;
- required node affinity `intel.feature.node.kubernetes.io/gpu=true`;
- non-root, with the render group as clustarr's pool sets it. On talos the
  render node is 0666, so no group is needed, but keep the value
  configurable;
- the media claim mounted read-only at `/media`, the path Plex's argv names.

**Start-up check:** each pod proves its device before it reports ready, as
clustarr's pool does with `inprocess.Engine.Measure`:
- an `h264_qsv` encode;
- a `vpp_qsv` scale;
- VA-API decodes, recording which formats decode on the GPU.

A pod whose device fails stays unready and takes no work.

**The pipeline**, per job, every step through ffgo (see Constraints):
1. **Decode** on the GPU through VA-API when the format was measured to
   decode there. Otherwise decode on the CPU (the NUC has 16 cores) and
   upload.
2. **Scale and convert the format** on the GPU with `vpp_qsv`, to Plex's
   `scale=w:h` and `format`.
3. **Burn in** subtitles, when the job asks for it:
   - Text subtitles (SRT, ASS) are rendered with libass, the `subtitles`
     filter or ffgo's `SubtitleRenderer`.
   - Image subtitles (PGS, VobSub) are overlaid.
   - Either way, then upload to the GPU.
   - CPU compositing is my call; `overlay_qsv` is the builder's option if it
     measures better.
4. **Encode** with `h264_qsv`:
   - Bitrate capped by Plex's `-maxrate` and `-bufsize`; quality derived
     from `-crf`.
   - Frame rate from `-r`.
   - An IDR frame every `segment_time` seconds, so each segment starts on a
     keyframe.
   - A low-latency configuration, so the first segment arrives quickly.
5. **Audio** as Plex asked: decode, resample or downmix, encode AAC at
   Plex's bitrate, or copy.
6. **Segment** into MPEG-TS files named exactly as Plex's pattern says,
   numbered from `-segment_start_number`, each cut on the forced keyframes.
   A DASH job with an encoded video uses the remux pool's DASH contract
   instead.

**Nothing on disk (owner, 2026-10-26).** The worker muxes every output in
memory and streams it back: an HLS or DASH segment whole, a whole-file job's
bytes as (offset, data) writes the relay applies to the output file. No
FFmpeg muxer writes a file on the NUC and there is no temporary output file.
The one file the worker holds is a subtitle the manager streams for burn-in,
in a memory-backed emptyDir, because FFmpeg's `subtitles` filter reads a file.

**Concurrency:**
- Each pod runs at most `maxJobs` conversions at once (default 3). `/readyz`
  reports not ready when full. *As built (2026-10-27, final review):*
  `/readyz` reports the device alone. A pod unready while full could not
  take a seek, which Plex restarts within milliseconds into its session's
  slot, and a fourth stream read as `no-pool`; a full pod answers a new
  session `ResourceExhausted`, which the route passes over, and refuses
  `full`.
- The route picks a ready pod with a free slot; the hash ring is used only
  to break ties.
- One pod with 3 slots meets the owner's capacity of 3. Replicas and
  `maxJobs` are values.
- The NUC's 4 GPU shares fit one pool pod beside clustarr's one.

**Whole-file jobs:** downloads, Optimize and the DVR's convert-while-recording
run the same decode, filter and encode, but write one output file. The
container is whatever the argv names, such as MP4 or MKV.
- Each of the three is captured before its parser shape is written.
- **The relay** streams the file in chunks, writes it into the job's output
  path under the session's directory (or the path Plex's argv names, if the
  capture shows one outside it), and renames it complete. It reports
  progress as Plex's transcoder does.
- **Pacing:** these jobs are not paced to a viewer. They run as fast as
  their slot allows.
- **Priority:** they count toward the Lease while running, so clustarr's
  Intel admission yields to them too.
- **Capacity:** they share `maxJobs` with streams. A whole-file job never
  takes the last free slot while a stream could need it: one slot is held
  back for streams.

**Pacing:** the pool keeps the pace PMS sets for its own transcoder,
running a throttle buffer ahead of the viewer rather than as fast as the
GPU allows (my call, after the review).
- **Why:** a free-running conversion at about 20× real time would write a
  whole film's segments into the Pi's transcode `emptyDir`, which has no
  size limit and sits on etcd's disk, and push them through the Pi's NIC,
  whose stalls already cause etcd elections.
- **The mechanism** by which PMS throttles its transcoder is captured in
  §2's step and reproduced, whether that's PMS's preference
  `TranscoderThrottleBuffer` or what its progress responses say.
- **Consequences:** a stream holds its slot for the whole viewing, "3
  streams" means 3 viewers, and clustarr's Intel admission stays paused for
  as long as anyone watches a converted stream.

**Seeking and restarts:**
- Plex restarts a job with a new `-ss` and `-segment_start_number`, and the
  worker starts from them.
- Cancellation follows the remux pool's: the shim's context, then the relay,
  then the worker, then the pipeline.

### 4. The relay: one more file contract

The relay keeps its rules: a file is written in full and renamed before Plex
is told about it, and errors become a non-zero exit with the reason.

What it adds:
- **HLS file names:** the names Plex's pattern allows, `media-\d{5,}\.ts`.
- **A segment-list event:** the relay POSTs it to the job's `-segment_list`
  URL in Plex's CSV form, one line per finished segment. It's sent only
  after that segment's file is in place, the order Plex's own segment muxer
  keeps.
- **Progress PUTs** as Plex's transcoder sends them.

**Exact formats:** the CSV line format, and the progress fields Plex reads
during a live conversion, are taken from Plex's own transcoder during the
capture step (§2). They are not assumed.

### 5. Live TV input

**The worker holds no Plex secrets** (settled after the review, keeping
ADR-0007's rule that a job reaches a worker without Plex's token or URLs):
the pool parses untrusted media.

- **Library files** the worker reads directly from its read-only `/media`
  mount, the path Plex's argv names.
- **Every other input is streamed by the manager.** That covers a Live TV
  conversion's live session (an HTTP URL on Plex) and a subtitle Plex
  downloaded itself into its config volume rather than beside the media.
  The manager reads the input on the worker's behalf (from Plex, with the
  session's token, or from the config claim it mounts) and streams the bytes
  over the job's gRPC call.
- **The proto** therefore carries input as well as output.
- **Network path:** the manager reaches Plex at its own address, never
  through the proxy's client-facing one.
- **Captured shape:** if the capture shows Live TV's input is something
  else, such as a file in the session directory or the recorder's stream
  URL, the manager streams that instead.

### 6. Priority over clustarr's batch encodes (owner's mechanism)

**The signal, written by cluster-plex.** cluster-plex's manager keeps a
Lease, `cluster-plex-video-transcodes`, in its own namespace. The Lease's
holder is the manager that last updated it. It carries:
- an annotation `clusterplex.io/active-video-transcodes: "<n>"`, the number
  of conversions running across the pool;
- a renew time.

Every manager updates it while it relays a conversion, at least every
10 s, and sets it to `0` when its last one ends. A Lease not renewed for
30 s counts as `0`.

Each manager keeps its own count in `clusterplex.io/video-transcodes.<pod>:
"<n> <unix seconds>"` and writes the annotation as the sum of the entries
renewed within 30 s, so one manager's last conversion ending cannot zero
another's (the plan's call; one manager is the talos case). clustarr's flag
is `--video-transcode-lease <namespace>/<name>`, chart value
`manager.videoTranscodeLease`, empty for no yield. clustarr's manager role
already grants get, list and watch on leases (for Live TV's), so the yield
adds no grant; a ClusterRole generated from markers cannot name one Lease.

**The yield, in clustarr.** clustarr's transcode admission
(`app/transcode/admission`, controller `transcode-admission`) reads that
Lease:
- the namespace and name come from flags, with chart values;
- while it reads a fresh count above zero, it admits no new file to the
  `intel` class;
- files already Running are untouched;
- once the count is zero or stale, admission resumes.

**Invariants.**
- **cluster-plex** writes only its own Lease.
- **clustarr's manager** only reads it, and so stays the only writer of
  clustarr's resources. Its role gains `get`/`watch` on that one Lease name.
- **Precedent:** clustarr already reads cluster-plex's `livetv-<device>`
  Leases.

### 7. The chart

cluster-plex's chart gains a `videoTranscode:` section:
- `enabled`, `image`, `replicas`, `maxJobs`;
- `nodeSelector`, `affinity` and `tolerations`, defaulting to the Intel NFD
  label;
- `gpu`, with resource name and count;
- `resources` and `renderGroups`.

It also gains the manager flags for the route (selector and port) and for
the Lease. The clustarr chart gains the admission flags for the Lease.

**Configuration rules:**
- With `videoTranscode.enabled: false`, video conversions are refused, not
  run on Plex's node; the owner's no-fallback rule holds either way.
- Enabling requires the clustarr chart that pulls this cluster-plex version.

## Constraints

- **Media work goes through ffgo only** (owner, 2026-10-26). The pool
  decodes, filters, encodes, renders subtitles and segments through the
  `mediactl/ffgo` fork's Go API: `Decoder` with a hardware device,
  `FilterGraph`, `Encoder`, the segmenter and `SubtitleRenderer`.
  - There are no cgo or purego calls into FFmpeg's libraries from
    cluster-plex, no FFmpeg CLI in any image, and nothing that execs FFmpeg.
  - FFmpeg filter and encoder names (`vpp_qsv`, `hwmap`, `h264_qsv`) appear
    only as what ffgo's filter graph and encoder are given, as clustarr's
    QSV tier gives them.
  - Where ffgo lacks something the pool needs, the fork is extended and
    tagged, then cluster-plex's pin is moved. Possible gaps: an option the
    QSV encoder needs, mapping VA-API frames for QSV, an MPEG-TS segment
    cut on forced keyframes, a subtitle overlay.
  - **The pin today:** cluster-plex pins `v0.0.0-clustarr.10`, and origin's
    tags stop at `.12`. The hardware decode support clustarr built is in
    `ffgo-unify`'s `v0.0.0-clustarr.13`: `StreamDecoderConfig.ExtraHWFrames`
    and `WithInterrupt`, which abandons a stalled read.
  - **Approved:** the owner approved pushing `v0.0.0-clustarr.13`, and the
    tags this work adds after it (2026-10-26). cluster-plex's pin moves to
    them.
- **Code:**
  - Go, with ffgo calls bounded by context as clustarr's `pkg/ffruntime`
    bounds them;
  - GPL-compatible dependencies only.
- **Deploying to talos:**
  - All talos deploys go through cluster-bootstrap's Terraform path: edit
    `clustarr/talos-values.yaml`, then `hack/clustarr-deploy.sh`.
  - A chart change means publishing new chart versions (clustarr pulls
    cluster-plex as a subchart) and setting `chart_version`.
  - Never use `helm upgrade` from the CLI.
- **The control plane stays light.** Plex's node is a Raspberry Pi
  control-plane node whose NIC stalls already trigger etcd elections.
- **Other work on the NUC.** clustarr's transcode pool and its other GPU
  users keep working there.

## Verifying it

**Unit tests.**
- **Parser:** against every recorded argv. Refusals are pinned by name, the
  way `TestAnUnknownOptionIsRefused` pins them.
- **Route:** a video conversion never reaches `Local` or `tryRemote`, and a
  refusal exits non-zero.
- **Relay:** files land before their segment-list line, names are checked,
  and errors are mapped.
- **Lease:** cluster-plex's count and expiry, and clustarr admission's yield
  and resume.

**The pipeline:**
- Tests run with a CPU H.264 encoder where no GPU is present, so CI covers
  segmenting, keyframes, naming, audio and burn-in.
- A GPU test runs only where an Intel render node is present, and fails,
  not skips, when it is expected to be there.

**On talos, before calling it done:**
1. Live TV on Plex for iOS: the pool's added start-up time per job, from
   the job's start to its first segment's file minus the input time that
   segment covers. Target: at most about 2 s. Record the tap-to-picture
   time too.
2. 30 minutes of one Live TV stream with no rebuffering, the pool's encode
   speed above 1×, and no `Plex Transcoder` process on plex-0's node during
   it.
3. Three conversions at once (two Live TV channels and one library film),
   while two clustarr Intel encodes are already running.
4. A library film with burned-in subtitles, an HDR film converted to SDR,
   and an interlaced Live TV channel.
5. A Plex for iOS download that needs conversion completes and plays
   offline.
6. clustarr's Intel admission pausing while streams are live and resuming
   within a minute after.
7. The NUC down or the pool scaled to 0: a tune is refused with an error and
   nothing runs on the Pi.

## Left to the builder

- **QSV encoder settings:** the rate-control mode, look-ahead, async depth
  and low-power choice within the start-up and quality targets.
- **Where burn-in is drawn:** CPU composite or `overlay_qsv`.
- **How to segment** (decided in the plan): one MPEG-TS muxer per job
  writing into memory, drained and cut at each forced keyframe the cut rule
  names; ffgo's HLS segmenter is not used, since it writes files.
- **Image subtitles** (decided in the plan): composited in Go onto the CPU
  frame from ffgo's decoded bitmaps; ffgo's filter graph is one linear chain,
  so FFmpeg's two-input `overlay` is not available.
- **Code layout:** package and binary names, and whether the HLS and video
  code shares packages with `pkg/remux`.
- **Proto:** whether it extends `remuxpb` or adds a sibling service.
- **Metric names and labels,** within cluster-plex's conventions.
- **The image build:** copying clustarr's Intel stage, or building a shared
  base.

## As captured (2026-10-26/27)

Plex's own transcoder was recorded on talos's plex-0 (`hack/capture-video-jobs.sh`): its argv, its process state, and its socket calls through strace. The fixtures are in `pkg/video/testdata` (`plex-video-jobs.jsonl`, `segment-lists/`, `progress/`, `clips/`), and the ledger's `Finding` lines hold the evidence counts.

- **L, the segment list.**
  - Every update is a separate chunked POST (`Connection: close`) of the whole list to `-segment_list`.
  - At a cut, the transcoder posts the finished list, then again with the next segment opened as `#name,start,start,start,start`.
  - A list holds at most `-segment_list_size` - 1 entries, newest last.
  - A line is `name,start,end,<stream ends>`. The stream ends come highest stream index first: the audio's, then the video's, even where the audio outlasts the video.
  - The answer is never read. Five shapes' lists replay byte for byte.
- **R, the cut rule.** Output frames sit on a constant-rate grid anchored at `-ss`, and keyframes are forced as that grid reaches multiples of `-force_key_frames`' interval. A segment ends at the first forced keyframe at or after (k+1) × `-segment_time` - `-segment_time_delta`, where k is the segment's number. A restart is `-ss 1800 -segment_start_number 225`. The rule holds at 8 s and at 5 s.
- **V, progress.**
  - A PUT to `-progressurl` about every 0.5 s on one keep-alive connection, carrying `progress`, `size=-1`, `remaining`, `speed` and `vdec_hw_status`.
  - PMS answers 206 with `<MediaContainer size="0"/>`.
- **P, throttling: `answer`.**
  - While the job is `TranscoderThrottleBuffer` ahead of what the client has fetched (8-9 segments, about 60 s), PMS answers the progress PUT with `canThrottle="1"`.
  - On that answer the transcoder logs `Throttle - Going into sloth mode.` to `<progress>/log` and stops producing. It keeps PUTting progress every 0.5 s, with no `speed` and `remaining=2147483647`.
  - At the first plain answer it logs `Throttle - Getting back to work.` and resumes: about one sloth/work cycle per segment, 1× in steady state.
  - PMS never stops the process. PMS also ends a session nobody reads from after about 180 s, so throttling shows only with a client reading.
- **H, HDR.** On the Pi, Plex asks no tone map (it has none on arm64): HDR10 goes through `scale` and `format=yuv420p|nv12` alone. The pool tone-maps from the source.
- **W, DASH with an encode.** Plex Web's job is the remux pool's DASH muxer: the same names, `-seg_duration 8`, `-window_size 5` and `-skip_to_segment 1`, with the video encoded. Each POST is FFmpeg's dynamic MPD.
- **B, burn-in.**
  - **An image (PGS):** `[0:N]scale=W:H[s];…;[v][s]overlay`.
  - **A sidecar:** PMS copies it into the session directory as `temp-0.srt`, a second `-i`, with `-map_inlineass 1:s:0`.
  - **An embedded text track:** `-map_inlineass 0:N` (the track's index).
  - **The renderer:** text burns through Plex's own `inlineass` filter. A sidecar's carries Plex's style overrides on a 1080-line scale; an embedded ASS keeps its own.
  - **Extra output:** each text burn-in ends with a second output, `-map … -f null -codec ass nullfile`.
- **Not yet captured.** These need the owner:
  - **I:** Live TV, a tune in Plex for iOS;
  - **D:** an interlaced channel;
  - **F:** a download, Optimize, and the DVR's convert-while-recording.
  
  Until each is captured, the parser refuses that shape as `shape`.
