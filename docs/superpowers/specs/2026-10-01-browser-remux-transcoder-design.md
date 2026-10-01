# Browser streams on our own transcoder

Date: 2026-10-01. Status: placement, engine and cache decided by the owner; awaiting review of this spec.

## Goal

Plex is a UI and API. clustarr probes, detects and transcodes, and the
settings that made Plex analyse media on its own are forced off
(`pkg/plex/prefs`, 2026-10-01). Since `TranscoderCanOnlyRemuxVideo` is on,
the one transcode Plex still runs is a browser's stream: Plex Web cannot
play Matroska, nor EAC3, AC3 or DTS audio, nor more than two channels, so
every browser session is a remux of the video plus an audio conversion to
stereo AAC, packaged as DASH.

This design runs that job on our own transcoder, on a pool that scales
apart from the Plex pods, and leaves every other job with Plex's
transcoder.

## What Plex asks for (captured 2026-10-01)

Every one of the 44 DASH jobs in plex-0's log has the same shape. The
Arcane episode's (AAC 5.1 source):

```
Plex Transcoder -codec:0 hevc -codec:1 aac [-ss 495] -noaccurate_seek
  -analyzeduration 20000000 -probesize 20000000 -i <file>
  -start_at_zero -copyts [-fps_mode cfr] -y -nostats -loglevel quiet -loglevel_plex error
  -progressurl http://127.0.0.1:32400/video/:/transcode/session/<s>/<u>/progress
  -map 0:0 -codec:0 copy
  -filter_complex "[0:1] aresample=async=1:ochl='stereo':rematrix_maxval=0.000000dB:osr=96000[0]"
  -map "[0]" -metadata:s:1 language=eng -codec:1 aac -b:1 256k
  -f dash -seg_duration 5 -dash_segment_type mp4
  -init_seg_name 'init-stream$RepresentationID$.m4s'
  -media_seg_name 'chunk-stream$RepresentationID$-$Number%05d$.m4s'
  -window_size 5 -delete_removed false -skip_to_segment <n>
  -manifest_name "http://127.0.0.1:32400/video/:/transcode/session/<s>/<u>/manifest?X-Plex-Http-Pipeline=infinite"
  -avoid_negative_ts disabled -map_metadata -1 -map_chapters -1 dash
```

Running Plex's own binary (from the image) against a recorder showed the
whole contract:

- **Segments are files in the working directory**, `init-stream{0,1}.m4s`
  and `chunk-stream{0,1}-NNNNN.m4s`, which PMS serves from its session
  directory. Nothing uploads them.
- **The manifest is POSTed**, chunked, to `-manifest_name` after every
  segment: ffmpeg's own DASH MPD (`User-Agent: Lavf`), `type="dynamic"`
  while running and `type="static"` with `mediaPresentationDuration` at
  the end, `startNumber` equal to `-skip_to_segment`.
- **Progress is PUTs** under `-progressurl`: `/progress/stream?index=…&codec=…&type=…`
  and `/progress/streamDetail?…` per input stream, then `?duration=`,
  `?width=&height=`, and `?progress=&size=&remaining=&speed=` as it runs.
- **A seek restarts the job** with `-ss <(n-1)·5>` and `-skip_to_segment n`.

Only `-progressurl`, `-manifest_name`, `-skip_to_segment`,
`-delete_removed` and `-loglevel_plex` are Plex's; the rest is stock
ffmpeg.

## Found on the way: remote dispatch cannot serve a DASH session today

`remoteexec` sends a job to a worker with the serving pod's working
directory as `cwd`. The transcode directory is a per-pod `emptyDir`, so on
the worker that session directory does not exist, the start fails before
any output, and the dispatcher runs the job locally. Every browser stream
has therefore run on the pod serving it. (From the code; plex-0's logs
since its last restart hold no transcoder job to confirm it.) The design
below has to put segments in the serving pod's directory, wherever the
job runs.

## Design

### 1. Detection (manager, `remoteexec`)

A `Classify(req) Route` runs before dispatch. A job is a **browser remux**
when all of:

- the target is `Plex Transcoder`, with `-f dash` and
  `-dash_segment_type mp4`;
- every video output is `-codec:N copy`, and every audio output is
  `copy` or `aac` (possibly through the `aresample` downmix above);
- no subtitle output and no video filter (no burn-in, no tone mapping);
- the playback session whose `TranscodeSession` key is the session id in
  `-progressurl` names its player's product `Plex Web` in PMS's
  `/status/sessions` (2 s timeout; unanswered means not a browser).

Anything else is Plex's, exactly as today.

### 2. The remux worker (`cmd/remux-worker`, ffgo in process)

No subprocess. The worker follows clustarr's in-process engine
(`app/squash/worker/inprocess`, `pkg/transcode/engine`): FFmpeg 9 loaded
through ffgo, the pipeline in Go, packets moving through channels and
bytes through `io.Writer`s.

- **Job.** The manager parses Plex's argv into a typed `remux.Job`
  (`pkg/remux`, which links no ffgo): input path, start time,
  `skip_to_segment`, the video stream to copy, the audio stream with its
  output channels, sample rate and bit rate, the segment duration and the
  session's progress and manifest URLs. Anything outside the shape in §1
  is refused, so Plex's transcoder runs it. Only the worker links ffgo:
  the manager and shim stay static binaries, for the reason
  `cmd/clustarr` never links ffgo (a dynamic loader the scratch image
  cannot start).
- **Segment boundaries are ours and deterministic.** Segment *n* starts at
  the first video keyframe at or after `(n−1) × seg_duration` in source
  time. A job starting at *n* seeks to the keyframe before that point and
  drops packets up to the boundary; one running from the start cuts at the
  same keyframes. Timestamps keep source time (`-copyts`), so a segment
  remuxed twice is the same segment, which is what lets the cache (§4)
  mix runs.
- **Pipeline.** One ffgo Decoder demuxes. Video packets are copied; audio
  is decoded, resampled to what the argv asks (stereo, its `osr`) and
  encoded to AAC at its bit rate. Packets fan out through channels to the
  muxers.
- **Muxers write to Go writers.** Each representation (video 0, audio 1)
  has its own ffgo Muxer producing fragmented MP4
  (`movflags=frag_custom+empty_moov+default_base_moof`) into an
  `io.Writer`. At each boundary the worker flushes a fragment on both, so
  a segment is exactly one `moof`+`mdat` per representation. A Go
  segmenter reads the init (`ftyp`+`moov`) and each fragment off the
  stream and emits `init-streamR.m4s` and `chunk-streamR-NNNNN.m4s`,
  numbered from `skip_to_segment` as Plex's binary numbers them.
- **Tee.** Each muxer's writer is an `io.MultiWriter`: every byte also
  appends to that representation's cache file. One demux and one audio
  encode feed the client and the cache together.
- **Manifest in Go.** The MPD is rendered from the segments produced, in
  the captured shape: a `SegmentTemplate` with the two name patterns,
  `startNumber`, and a `SegmentTimeline` of each segment's duration in the
  representation's timescale; `dynamic` while running, `static` with
  `mediaPresentationDuration` at the end.
- **Progress** PUTs (`stream`, `streamDetail`, `duration`,
  `width`/`height`, `progress`/`remaining`/`speed`) come from the
  pipeline's own counters.

ffgo gains two things in the mediactl fork, each offered upstream:
`NewMuxerToWriter(w io.Writer, format string)` over its existing
`CustomIOContext` (write-only, not seekable), and `Muxer.Flush()`
(`av_write_frame(ctx, NULL)` then `avio_flush`) for `frag_custom`.

### 3. Placement and transport

The **remux pool** is a Deployment running `cmd/remux-worker`: an image
with FFmpeg 9 and the ffgo shim (clustarr's transcoder image pattern), no
Plex, its own label, an HPA on CPU. It serves a gRPC service of its own:

```proto
service Remux {
  rpc Remux(RemuxJob) returns (stream RemuxEvent);  // File{name,data} | Manifest{xml} | Done{error}
}
```

The serving pod's manager, which received the job from the shim:

- picks the worker by consistent hash of the job's cache key
  (`pkg/hashring`), so a file returns to the worker holding its cache;
- writes each `File` into the job's `cwd` (names checked against the two
  patterns, no separators);
- POSTs each `Manifest` to its own PMS only **after** the files it lists
  are written, so PMS never lists a segment it cannot serve;
- answers the shim with exit code 0 on `Done`, non-zero on an error, as
  Plex's transcoder would.

Progress PUTs go from the worker straight to the serving pod's PMS, at the
address the dispatcher already rewrites `127.0.0.1` to. With no ready
remux worker the job goes to Plex's transcoder: Plex pods carry no
FFmpeg 9.

### 4. Remux cache

- **Key:** the input path with its size and modification time (read by the
  worker), the audio stream index, and the output parameters (channels,
  sample rate, bit rate, segment duration). A changed file or a different
  audio choice is a different entry.
- **Where:** a worker-local volume (an `emptyDir` with a size limit, or a
  local PersistentVolume), never NFS. The hash ring sends a key to one
  worker; a worker leaving the ring loses its share, which is acceptable
  for a cache.
- **Layout** per key: `video.mp4` and `audio.mp4`, fragmented MP4s (the
  init, then fragments appended in the order produced), and `index.json`
  mapping each segment number to its byte range, duration and start time
  in each file, plus the init's range. A run that started after a seek
  appends as well; the index finds segments by number wherever they lie.
  The index is rewritten by rename after the fragment's bytes are synced.
- **Serving:** a job for segment *n* is answered from the cache for as long
  as the index holds *n*, *n+1*, …; at the first gap the pipeline starts
  at that segment's boundary and tees from there. A file played through
  once plays again with no decoding.
- **Eviction:** least recently used, once the volume passes a high-water
  mark.

### 4a. Failure

- Classifier, parse or session lookup fails, or no remux worker is ready:
  Plex's transcoder.
- The worker fails before its first `File`: the manager runs Plex's
  transcoder on the same request.
- Fails after: `Done` carries the error, the shim exits non-zero as a
  failed Plex transcode would, and the client's retry re-runs the
  classifier.
- PMS kills a session (stop or seek): the shim's context ends, the manager
  cancels the stream, and the worker's context stops the pipeline. A
  partly written fragment never reaches the index.

### 5. Tests

- ffgo (in the fork): a writer-backed Muxer round-trips through a decoder;
  each `Flush` emits exactly one `moof`.
- Classifier and parser: a table from the 44 real argvs in plex-0's log,
  plus a burn-in, an HLS (`-f segment`) and a video transcode, each with
  its route and parsed `Job`.
- Pipeline, on a generated clip with known keyframes: segments decode,
  start at the boundary keyframes, are numbered from `skip_to_segment`,
  and a seek run's segment *n* is byte-identical to a full run's.
- Cache: a second run serves every segment with no decode (counted); a gap
  is filled by a pipeline started at its boundary; a new mtime is a miss;
  eviction removes the least recently used key.
- Protocol: against a recording PMS, what Plex's own binary was recorded
  doing: file names, every listed segment present before its manifest
  POST, `startNumber` after a seek, the progress keys.
- Transport: the manager writes streamed files into `cwd`, posts the
  manifest only after them, refuses a name outside the patterns.
- e2e on kind: Plex Web plays a file through the pool (the remux pod's
  log shows the job, PMS serves its segments); a replay is served from
  the cache.

## Out of scope

- Browsers that cannot decode HEVC: they need a real-time video transcode,
  which this does not do.
- Other clients' jobs (HLS `-f segment`, progressive MKV): Plex's.
- Subtitles: text sidecars are delivered by PMS; image subtitles cannot be
  burned in with video transcoding off.

## Decided (2026-10-01)

1. Placement: the remux pool, with segments streamed back to the serving
   pod.
2. Engine: ffgo in process, after clustarr's engine, not a subprocess, so
   the output is Go streams the client and the cache share.
3. The MP4 written beside each stream is a remux cache on the pool, not a
   library version.
