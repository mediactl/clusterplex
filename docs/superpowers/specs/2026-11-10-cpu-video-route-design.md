# Plex's conversions on the NUC's CPU while clustarr has encodes waiting

**Status:** approved by the owner on 2026-11-10 ("Approve CPU-route re-design
with a caveat: do an in-depth comparison to reason whether we see performance
degradation and if it is noticeable for a user of the plex server"). The
comparison is measured, below ("Measured: the CPU against the GPU, for the
viewer"), and sizes §2. Nothing is built yet.

## Why

The owner (2026-11-05): "Can we force plex to use the cpu transcoder *on*
the NUC node while transcode items exist?"

Plex's video conversions run on the Intel NUC's GPU pool
(`2026-10-26-gpu-video-transcode-pool-design.md`, ADR-0010), and so do
clustarr's batch encodes. The GPU (an Arc 140T) has two video engines; a
conversion to a viewer runs in real time and must never be squeezed. Since
clustarr d9a6f7d4 (talos rev 223) clustarr's admission reserves the engines
the streams need -- a stream a quarter of one, rounded up to whole engines
-- and runs one encode beside one to four streams (`VideoAllowance`). The
second encode slot is lost for as long as anyone watches a converted stream,
which on 2026-11-04 was a whole evening with 32 files waiting.

Meanwhile the NUC's 16 cores sit nearly idle: 4% busy on 2026-11-10 with a
conversion running, clustarr's two QSV encodes about a core each. A 1080p
software encode for one viewer costs two or three cores. Moving the
conversions there while clustarr has work gives clustarr both engines (the
QSV benchmark of 2026-11-05: 22 MB/s of source at one encode, 29 at two) and
costs the viewer nothing visible, provided the CPU keeps real time.

## What the owner decided

- Plex's conversions move to the CPU **on the NUC** while clustarr has
  transcode items (owner, 2026-11-05).
- The GPU stays the route when clustarr is idle, and the yield
  (`VideoAllowance`) stays as the fallback.

Still the owner's to confirm (my recommendation in each):

- **Which conversions move:** sources at 1080p or below without HDR, whose
  video is H.264 or MPEG-2. A 4K or HDR source stays on the GPU under
  today's yield: a software tone map or a 4K decode in real time is where
  the CPU may not keep up. An HEVC, AV1 or VP9 source stays on the GPU too,
  where VA-API decodes it: a software HEVC Main 10 decode at 1080p is a
  core budget of its own before libx264 starts, and clustarr has made most
  of the library HEVC, while the conversions that hold the GPU for an
  evening are Live TV's H.264 and MPEG-2 channels.
- **Whole-file jobs stay on the GPU:** a download for Plex for iOS, an
  Optimize or the DVR's convert-while-recording is unpaced and would hold a
  CPU slot at the pod's limit for a film's length beside a viewer's stream.
  They run on the GPU and keep counting toward the Lease, as the pool design
  §3 has them.
- **The CPU pool as a second chance:** an eligible conversion the GPU pool
  refuses as full runs on the CPU pool rather than fail, and the other way
  round. ADR-0010's "no fallback" was about Plex's own node, which still runs
  nothing.

## Goals

- clustarr's intel class keeps both slots while a viewer's conversion runs
  on the CPU pool.
- A conversion on the CPU pool is indistinguishable to the viewer: it
  starts as fast and holds 1.0x.
- Nothing moves mid-stream, and nothing new for the operator to run by hand.

## Non-goals

- Moving a running conversion between pools.
- Any change to what clustarr encodes or how it is admitted beyond the one
  signal below.
- Software conversion of 4K or HDR sources (left on the GPU).
- Plex's remuxes and audio-only conversions, which never touched the GPU.

## Anti-goals: a failure even if it works

- A stream that stutters on the CPU pool. Capacity is measured before the
  values are set, and a pod that cannot hold real time takes fewer jobs.
- The CPU pool starving the usenet engine's par2 repairs or clustarr's
  encodes on the same node. Its cores are requested and limited.
- clustarr counting a CPU conversion as a GPU stream and yielding an engine
  for nothing.

## What exists, and the gaps

- **The video worker already converts on the CPU:** `--encoder libx264`
  decodes, deinterlaces (bwdif), tone-maps (zscale), scales and encodes
  in software with no GPU, `preset veryfast`, CRF from Plex's `-crf`, and
  `measureDevice` skips the GPU proof for it. The chart exposes it as
  `videoTranscode.encoder`, for the whole pool.
- **The manager finds one pool** by one selector (`--video-selector`,
  `app=plex-video-transcode`) and dials `--video-port`; the relay
  (`pkg/video/relay.Relay.run`) lists its ready pods, tries the hash
  ring's pick first, passes over a full pod (`ResourceExhausted`), refuses
  the job on a shape the worker cannot run (`InvalidArgument`).
- **The count clustarr reads** is `pkg/video/lease.Counter`: every
  conversion the relay runs is counted into the Lease
  `cluster-plex-video-transcodes` (`clusterplex.io/active-video-transcodes`),
  renewed every 10 s, stale after 30 s.
- **clustarr's admission** (`app/transcode/admission`) reads that Lease
  (`videoStreams`) and sizes the intel class's budget by `VideoAllowance`.
  Its pass already knows what the signal needs: the intel class's slots in
  use (`running`) and the Planned files assigned to intel this pass
  (`queued`, held or unadmitted). Its role grants `get`, `list`, `watch` on
  Leases cluster-wide and, through the leader-election Role in its own
  namespace, `create`, `update` and `patch` there.
- **cluster-plex reads clustarr's namespace already**: it creates and
  updates the `livetv-<device>` Leases there (the chart's `plex.clustarr`
  Role: Leases `get`, `create`, `update`).
- **The gaps:** a second pool, a route that chooses between the two per
  conversion, a signal from clustarr, eligibility, and a Lease count that
  counts the GPU alone.

## Design

The calls below are mine unless marked as the owner's. A builder may change
a call if the code argues for it, and should say so.

### 1. The signal: clustarr says how much intel work it holds

clustarr's manager keeps a Lease, `clustarr-intel-work`, in its own
namespace (flag `--intel-work-lease <name>`, chart value
`manager.intelWorkLease`, empty for none), carrying one annotation,
`clustarr.io/intel-work: "<n>"`, where n is the intel class's files that
hold or want a slot: Queued, Running and Swapping files of the class, plus
the Planned files the pass assigned to intel (admitted or not, the yield's
holds included). Grafts and the cpu and nvidia classes are not counted.

- The admission controller writes it after a pass whenever n changed, and a
  renewer (the mirror of cluster-plex's `lease.Counter.Run`) renews it every
  10 s while n is above zero. A Lease not renewed for 30 s reads as 0, so a
  dead manager never holds Plex on the CPU. A write that fails is logged
  and retried at the next renewal; admission never waits on it.
- The write goes through the manager's own client as its one more
  Kubernetes write (the manager is the only writer, ADR-0019): no agent
  touches it. No new RBAC: the leader-election Role already grants the verbs
  in the manager's namespace.
- Why a Lease and not clustarr's TranscodeProfile status: `waitingFiles`
  counts the backlog beyond the window, not the class's queue, and reading
  a clustarr CRD from cluster-plex adds a client and a grant for one number.

### 2. The CPU pool: a second Deployment of the same worker

`videoTranscode.cpu` in cluster-plex's chart renders a second Deployment of
the video image with `--encoder libx264`, no GPU resource, label
`app=plex-video-transcode-cpu`, and the same inputs emptyDir and read-only
media mount:

- **Placement:** required node affinity on the NUC's hostname (the owner's
  "on the NUC node"; the only other large amd64 node, talos-9c, is shelved
  for its cooling). `nodeSelector`, `affinity` and `tolerations` are values,
  as the GPU pool's are.
- **Size (from the measurements below):** `maxJobs 2`, requests 3 CPU and
  2Gi, limits 8 CPU and 4Gi. The unit is a 1080p60 or 1080i60 H.264 Live TV
  channel to 720p at Plex's 9 Mbps cap, libx264 `veryfast`, held at 1.0x:
  measured at 1.0-1.25 CPU-seconds per media second (deinterlace and 60 fps
  included), so two such streams at real time are about 2.5 cores, with the
  8-core limit leaving a start-up burst (a tune runs flat out until Plex
  throttles it) and a third stream. The usenet engine (par2 repairs,
  multi-core bursts) and clustarr's two intel pods (about a core each) share
  the node; the request keeps the pool's cores its own and the limit keeps
  the node's. `libx264` is given `threads` equal to the limit, since its
  default is 1.5x the node's 16 and the pod sees 8.
- **Eligibility is the worker's, from the source it opens** (flags
  `--max-source-pixels 2073600` -- 1920x1080 --, `--no-hdr` and
  `--sources h264,mpeg2video`, the video codecs it takes): the stream's
  codec and size and its transfer characteristics (PQ or HLG is HDR), read
  from the stream's parameters before the first frame is decoded. A source
  over the limit, HDR, or of another codec is answered `FailedPrecondition`
  with the reason before any output, which the relay reads as "not for this
  pool" and passes over, never as a refusal of the job. A Live TV input
  (streamed MPEG-TS) is judged the same way from its first packets; the
  worker accepts a job before it opens its source, so the pass-over costs a
  dial and the first packets, and the manager's Live TV follower restarts
  from the playlist's first segment for the next pool. The worker is the
  judge because it alone has the source; the manager's pre-check is below.
  The codec list is what the pool is sized by: its defaults are measured on
  a Live TV channel, and a builder who widens the list (HEVC 8-bit, say)
  measures that case and sizes for it.
- **Only a streamed output goes to the CPU pool** (HLS, DASH): a whole-file
  job is routed to the GPU pool alone, whatever `when` says.
- The GPU pool is unchanged.

### 3. The route: which pool first

The manager gains `--video-cpu-selector` (chart: rendered from
`videoTranscode.cpu.enabled`; empty means no CPU pool), `--clustarr-work-lease
<namespace>/<name>` (the Lease of §1, read with the same staleness rule as
clustarr reads ours) and `--video-cpu-when` with three values:

- `clustarr-busy` (the default): the CPU pool first while the Lease is fresh
  and above zero;
- `always`: the CPU pool first for every eligible conversion;
- `never`: the GPU pool first, the CPU pool only as the second chance.

`Relay.run` becomes: parse the job; if the job is a whole-file output, or
its parse already says the source is HDR (`Video.ToneMap`), the CPU pool is
out; list the pools; order them by the rule; try the first pool's ready pods
as today (hash ring, `ResourceExhausted` passed over, `FailedPrecondition`
passed over), then the second pool's; refuse as today when neither takes it
(`full` when any pod was full, else the last error). The pool a conversion
lands on is recorded in its placement (`Placement.Pool`), named in the
"video conversion started" log line and in Plex's activity (a CPU conversion
already shows as software, since `Placement.Encoder` is `libx264`).

The signal's count, on clustarr's side, includes the files the yield holds
class-less (`ChooseClass` leaves a waiting `gpu` file without a class until
a slot opens): they are intel work waiting, whatever their class reads.

A manager-side pre-check -- the library file's probed size and HDR from the
projection, or Plex's own media_streams -- would save the CPU pool an open
on an ineligible source. It is an optimization left to the builder; the
worker's judgement is the rule.

### 4. The count: GPU conversions alone

`Count.Begin` runs only for a GPU placement. A CPU conversion is never in
`cluster-plex-video-transcodes`, so clustarr's `VideoAllowance` sees only
the streams that share its engines. Nothing else in clustarr's yield
changes; it stays the fallback for a conversion that landed on the GPU (the
CPU pool down, full, or the source ineligible).

### 5. How the two signals settle

- clustarr has work, a viewer starts a stream: the stream goes to the CPU
  pool; the GPU count stays 0; clustarr admits two encodes.
- clustarr has work, the CPU pool is full: the stream goes to the GPU and is
  counted; clustarr's next pass runs one encode beside it, as today.
- clustarr is idle, a stream runs on the GPU, then clustarr queues work: the
  stream stays on the GPU until it ends (nothing moves); the next stream
  goes to the CPU pool. clustarr's signal and cluster-plex's count both
  read "busy" meanwhile, which is correct: one encode runs beside the
  stream.
- A seek restarts the job (a new `-ss`); the restart is routed afresh and
  follows the same rule, so a stream may move pools at a seek. That is the
  one "move", and the viewer sees a restart either way.

### 6. The chart

cluster-plex's `videoTranscode.cpu`: `enabled` (false), `replicas` (1),
`maxJobs` (2), `resources`, `nodeSelector`, `affinity` (the NUC),
`tolerations`, `when` (`clustarr-busy`), `maxSourcePixels` (2073600), `hdr`
(false), `sources` (`[h264, mpeg2video]`) and `clustarrWorkLease`
(`<clustarr namespace>/clustarr-intel-work`, empty for none). The manager's flags render from them. The clustarr chart's
`manager.intelWorkLease` names the Lease it writes. Enabling the CPU pool
needs the clustarr chart that pulls this cluster-plex version, as the GPU
pool did.

### 7. What to watch

- `clusterplex_video_jobs_routed{pool}` and the existing refusals by reason,
  plus `clusterplex_video_pool_passed_over{pool,reason}` for a full pod or an
  ineligible source.
- The CPU worker's `clusterplex_video_worker_encode_speed`: a histogram
  bucket under 1.0 on a stream is the anti-goal showing.
- clustarr's `clustarr_transcode_jobs_active{class="intel"}` at two while a
  CPU conversion runs is the goal showing.

## Measured: the CPU against the GPU, for the viewer (2026-11-10)

The owner's caveat on approval was to establish whether a viewer would notice
the move. Measured on talos-88 (the NUC: 16 threads, Arc 140T) in a pod with
the pool's security context, one GPU share and an 8-CPU limit, through
ffmpeg 9.0.2's CLI (lscr.io/linuxserver/ffmpeg) with the parameters PMS asked
for in the recorded jobs and the worker's own encoder options (libx264
`veryfast`, CRF 16, maxrate 9000k, bufsize 18000k, no B-frames, a forced IDR
every second, scenecut off; h264_qsv at 80% of maxrate with the same GOP).
Inputs: 60 s of TNT as the relay serves it (1080p59.94 H.264, 6.8 Mbps), 60 s
of ESPN (720p59.94, 4.9 Mbps) and 60 s of a 1080p24 x264 film (library
playback to 720x404 at Plex's 1.8 Mbps). No 1080i channel was on the air in
the capture, so bwdif over the progressive 1080p is the deinterlace cost, which
is the same work. The node was otherwise idle (0.3 cores). Raw results are in
the session's scratchpad (`bench/results.txt`, `bench.sh`).

**Speed, one conversion, free-running (what a tune's burst and a seek get):**

| Conversion | libx264 on 8 CPUs | CPU-s per media s | h264_qsv | host CPU per media s |
| --- | --- | --- | --- | --- |
| 1080p60 to 720p30 | 8.2x | 0.97 | 15.8x | 0.05 |
| 1080p60 to 720p60 | 6.3x | 1.25 | 15.8x | 0.06 |
| 1080 deinterlaced to 720p30 | 5.9x | 1.21 | 15.9x | 0.05 |
| the same with Plex's own x264 tweaks (`subme=0`, `me=dia`...) | 10.1x | 0.78 | -- | -- |
| 720p60 to 720p30 / 720p60 | 12.9x / 8.9x | 0.61 / 0.88 | 31x / 22x | 0.04 |
| library 1080p24 to 720x404 at 1.8 Mbps | 27x | 0.28 | 39x | 0.03 |

**Speed under the pool's concurrency (deinterlaced 1080 to 720p30, 8 CPUs):**
two at once 3.45x each (4.0 cores each), two 60 fps ones 2.9x each, three at
once 2.3x each (2.7 cores each). Holding real time is what a stream needs,
and the pool's second slot leaves it a 3x margin; a third stream still has
2x. The GPU's two video engines are not shared with any of this.

**Tune-in (time to each 1 s segment of a deinterlaced 1080 conversion, from
a cold start):** libx264 has the first segment at 0.55 s and eight by 1.8 s;
h264_qsv the first at 0.22 s and eight by 0.62 s. Plex plays once two or
three segments stand, so the CPU adds about 0.6 s to a tune or a seek,
against the relay's 1.8 s to the upstream's first byte and PMS's own
seconds of tune latency: below what a person can tell apart.

**Picture, at the same bitrate cap, against the same CPU-scaled lossless
reference (VMAF / PSNR / SSIM):** live 1080 to 720p30: libx264 81.6 / 29.27 /
0.961 at 6.6 Mbps, Plex's own tweaks 81.0 at 6.9 Mbps, h264_qsv 80.5 / 29.27 /
0.961 at 7.5 Mbps; library 720x404: libx264 87.5 / 35.90 / 0.980, h264_qsv
87.2 / 35.80 / 0.978. The CPU output is a point of VMAF better while using
fewer bits (h264_qsv is driven at 80% of the cap and spends it); within a
point is invisible.

**Heat and neighbours:** the package reached about 73 C in the free-running
bursts (8 cores) and settled at 58 C a minute later; at the 2.5 cores two
real-time streams need it stays far lower. The usenet engine's par2 repairs
and clustarr's encodes keep their shares by the pod's request and limit.

**Verdict:** for the conversions in scope (H.264 and MPEG-2 sources at 1080p
or below, no HDR) the CPU route costs the viewer nothing noticeable: real
time with a 3x margin at the pool's two slots, about half a second more at a
tune or a seek, and equal-or-better picture at the same bitrate. What was not
measured, and stays excluded or watched: HEVC and HDR sources (excluded by
§2), a burned-in subtitle's libass cost on the CPU (modest; the builder
checks it in the §Verifying tunes), and the worker's own gRPC streaming
overhead, which is the same on both pools.

## Constraints

- Media work goes through ffgo only; the CPU path is the worker's existing
  software pipeline, not a new one.
- The worker holds no Plex secret; Live TV input is streamed by the manager
  as today (§5 of the pool design).
- The CPU pool pod writes nothing to disk (the pool design's rule): outputs
  are muxed in memory and streamed back.
- clustarr's manager stays the only writer of its resources; the Lease is
  its own, in its own namespace, and cluster-plex only reads it.

## Verifying it

Unit: the route's order for every value of `when` with the Lease fresh,
stale and absent; `FailedPrecondition` passed over to the other pool and
counted; the count taken for the GPU alone; clustarr's n counted from a
pass with Running, Queued and Planned intel files and none for grafts or
other classes.

On talos (there is no GPU on the rig):

1. `when: always` first, without clustarr's signal: tune a Live TV channel
   on the Apple TV, see the placement on the CPU pod, speed at 1.0x in the
   worker's log, the Lease count at 0, and clustarr admitting two encodes
   beside it. Seek, and see the restart land on the CPU pod too.
2. `when: clustarr-busy`: with clustarr's window empty the same tune lands
   on the GPU and is counted; queue encodes (a profile with files
   Planned), start a second tune, see it land on the CPU pod while the first
   stays on the GPU.
3. Two streams on the CPU pod at once hold 1.0x each; a third goes to the
   GPU as full. A 4K HDR film from Plex Web, and a 1080p HEVC episode played
   on Plex for iOS, land on the GPU after the CPU worker passes them over,
   each pass-over counted; a download for iOS never asks the CPU pool.
4. Scale the CPU pool to zero: every conversion runs on the GPU and clustarr
   yields, as before this design.

## Left to the builder

- The exact `FailedPrecondition` detail text and where the worker reads the
  transfer characteristics (the decoder's first frame, or the stream's
  parameters before decoding).
- The renewer's placement in clustarr (a runnable beside the admission
  controller, leader-only) and the Lease's holder identity (the pod name,
  as cluster-plex does).
- The CPU pool's measured capacity: run the §Verifying tunes with the
  defaults, read the worker's speed, and raise or lower `maxJobs` and the
  CPU request from what the NUC shows.
- Whether the manager's pre-check is worth its code once the pass-over is
  measured.
