# Browser streams on our own transcoder

Date: 2026-10-01. Status: proposed, awaiting approval.

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

### 2. Our transcoder (`cmd/remux`, new)

A Go binary that takes Plex's argv, so the classifier hands the request
over unchanged:

- Parses the argv into a typed job and refuses anything outside the shape
  above (exit code distinct from ffmpeg's, so the manager falls back).
- Runs stock ffmpeg with the stock options: the same inputs, maps, filter
  and audio settings; `-f dash` writing the manifest to a local file;
  `-progress pipe:` for progress.
- **Renumbers after a seek:** stock ffmpeg numbers from 1, so each
  finished segment is renamed `k → k + skip − 1` and the manifest's
  `startNumber` rewritten to `skip` before it is published.
- Publishes each manifest version only after the segments it lists are in
  place, translates `-progress` into Plex's PUTs, and sends the stream
  descriptions once at start, from ffprobe of the input.

The ffmpeg is a static upstream build in the worker image. clustarr's
in-process engine (ffgo) could replace it later behind the same binary.

### 3. Placement and transport

`cmd/remux` runs on a **remux pool**: a Deployment, its own image (ffmpeg,
no Plex), selected by its own label, with an HPA on CPU. The existing
gRPC `ExecuteRemote` carries the job there. Because the segments must
reach the serving pod, the worker never writes them to a shared path and
never talks to PMS about the manifest:

- `TranscodeLog` gains a `File{name, data, last}` message. The worker
  streams each finished segment and each manifest version back on the
  stream it already has.
- The serving pod's manager writes segment files into the job's `cwd`
  (names checked against the two patterns, no separators) and POSTs each
  manifest to its own PMS **after** the files it lists are written. PMS
  never lists a segment it cannot serve.
- Progress PUTs go straight from the worker to PMS through the rewritten
  `-progressurl`, as today.

With no ready remux worker, the job runs on the serving pod, through the
same code path, so nothing depends on the pool for correctness.

### 4. Failure

- Classifier or session lookup fails: Plex's transcoder.
- `cmd/remux` refuses the argv, or fails before its first segment: the
  manager runs Plex's transcoder on the same request.
- Fails after: the stream ends with ffmpeg's exit code, as a failed Plex
  transcode would, and the client retries, which re-runs the classifier.
- PMS kills a session (stop or seek): the manager cancels the stream; the
  worker kills its ffmpeg process group.

### 5. Tests

- Classifier: table from the 44 real argvs in plex-0's log, plus a burn-in,
  an HLS (`-f segment`) and a video transcode, each with the expected
  route.
- `cmd/remux` argv parser and ffmpeg argv: golden files from the same
  commands.
- Protocol: a contract test runs `cmd/remux` on a generated clip against a
  recording PMS and asserts what Plex's binary was recorded doing: file
  names, manifest POST order (every listed segment present first),
  `startNumber` after a seek, progress keys.
- Transport: the manager writes streamed files into `cwd` and posts the
  manifest only after them; a name outside the patterns is refused.
- e2e on kind: Plex Web plays a file through the pool (a remux pod's log
  shows the job, PMS serves its segments).

## Out of scope

- Browsers that cannot decode HEVC: they need a real-time video transcode,
  which this does not do.
- Other clients' jobs (HLS `-f segment`, progressive MKV): Plex's.
- Subtitles: text sidecars are delivered by PMS; image subtitles cannot be
  burned in with video transcoding off.

## Decisions for the owner

1. Placement: the separate remux pool with segments streamed back
   (recommended), or run `cmd/remux` only on the serving pod and keep
   scaling to the proxy spreading sessions across Plex pods (simpler, no
   proto change).
2. Engine: stock ffmpeg now (recommended), ffgo later.
