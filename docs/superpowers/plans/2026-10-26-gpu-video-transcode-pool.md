# GPU Video-Transcode Pool Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every video conversion Plex asks for runs on an Intel QSV pool on the NUC through ffgo, and is streamed back to the serving pod's manager, which hands it to Plex exactly as Plex's own transcoder would. Nothing converts video on the Pi, and clustarr's Intel admission yields while conversions run.

**Architecture:**
- **Routing.** The shim's `Plex Transcoder` job reaches the manager's dispatcher. A new `Video` route, after `Remux` and before `tryRemote`/`Local`, takes every job whose argv converts video. It parses the job against an allow-list built from captured Plex jobs, then opens a bidirectional `Video.Convert` gRPC stream to a ready worker with a free slot.
- **The worker** (`cmd/video-worker`, amd64, Intel runtime):
  - decodes on VA-API or the CPU;
  - scales on `vpp_qsv`;
  - burns subtitles in on the CPU when asked;
  - encodes `h264_qsv` with forced keyframes;
  - muxes every output in memory and streams it back. HLS segments come from one MPEG-TS muxer cut at those keyframes, DASH goes through the remux pool's fMP4 splitter, and whole files go as offset writes.
- **The manager** streams non-`/media` inputs up to the worker. It writes each output file into Plex's session directory and only then tells Plex. It also keeps a Lease that clustarr's transcode admission reads.

**Tech Stack:** Go 1.27; ffgo (the mediactl fork, `v0.0.0-clustarr.13`, then `.14` from Task 3); FFmpeg 9 (BtbN gpl-shared) with Intel's iHD driver and oneVPL runtime; gRPC/protobuf; client-go Leases; Helm; controller-runtime (clustarr).

**Spec:** `docs/superpowers/specs/2026-10-26-gpu-video-transcode-pool-design.md`, approved by the owner on 2026-10-26.
- Task 0 amends it with the owner's no-disk ruling and records my calls.
- Task 1 appends the "As captured" findings.
- Read the spec with both amendments.

## Global Constraints

**Repositories.** Other sessions commit in all three:
- cluster-plex: `/home/appkins/src/mediactl/cluster-plex`, module `github.com/mediactl/clusterplex`, branch main.
- The ffgo fork: `/home/appkins/src/mediactl/ffgo-unify`, module path `github.com/obinnaokechukwu/ffgo`. It is published as `github.com/mediactl/ffgo` with tags `v0.0.0-clustarr.N`, and work happens on branch `clustarr/unify-media`, where `.13` is.
- clustarr: `/home/appkins/src/mediactl/clustarr`, main, primary checkout only.

**Git.**
- Commit with a pathspec (`git commit -m '…' -- <paths>`), and `git add` every new file first.
- Never `git stash`. No worktrees. No `go get`, no `go mod tidy`.
- Push nothing except the ffgo tags `v0.0.0-clustarr.13` and `.14`, which the owner approved on 2026-10-26.
- Any other push, image push or chart publish waits for the owner's OK (Task 19).

**ffgo only (owner).**
- The pool decodes, filters, encodes, renders subtitles and muxes through ffgo's Go API.
- No cgo or purego of cluster-plex's own, no FFmpeg CLI in any image, and nothing that execs FFmpeg.
- Tests may run the `ffmpeg` CLI to make clips, as `pkg/remux/pipeline/pipeline_test.go`'s `clip` does.
- Only `cmd/video-worker` and the packages it alone imports (`pkg/video/pipeline`, `pkg/video/gpu`, `pkg/video/worker`) link ffgo. `cmd/manager/noffgo_test.go` holds the manager, shim, proxy and maintenance binaries to that.

**Nothing on disk on the worker (owner, 2026-10-26).**
- Every output is muxed into memory and streamed: `ffgo.NewMuxerToWriter`, or `ffgo.NewMuxerToWriteSeeker` from Task 3.
- No FFmpeg muxer is given a file path on the NUC, and there is no temporary output file.
- The one exception is a subtitle that the manager streams for burn-in. It is held in the memory-backed `emptyDir` at `--inputs-dir` (default `/run/video-inputs`), because FFmpeg's `subtitles` filter reads a file.

**No fallback (owner).**
- A job for which `video.IsConversion` is true never reaches `tryRemote` or `Local`.
- A refusal exits 1, with `video: <reason>: <detail>` on stderr.
- The reasons are exactly `no-pool`, `full`, `shape`, `worker-error` and `cancelled`, and they are the only values of the `reason` label.

**The worker holds no Plex secrets.**
- The `Job` on the wire carries no token and no URL.
- The manager streams every input that is not under `/media`, reading it from Plex at the manager's own address with the job's `X_PLEX_TOKEN`.

**Plex can't tell.**
- HLS file names match `^media-\d{5,}\.ts$` and are numbered from `-segment_start_number`.
- A segment-list line is sent only after its file is in place.
- Progress reports carry only finding V's paths and keys.
- DASH keeps `^(init-stream[01]|chunk-stream[01]-\d{5,})\.m4s$`.

**The Lease.**
- Name `cluster-plex-video-transcodes`, in the manager's namespace.
- Sum annotation `clusterplex.io/active-video-transcodes: "<n>"`, plus per-manager entries `clusterplex.io/video-transcodes.<pod>: "<n> <unix seconds>"`.
- Renewed at least every 10 s while any conversion runs.
- A Lease or an entry older than 30 s counts as 0.

**Capacity.**
- `maxJobs` defaults to 3 per pod.
- A whole-file job needs at least 2 free slots, so one slot is always held back for streams.
- A job for a session that already holds a slot takes that slot over and cancels the old job.

**Secrets.**
- Never print, log or commit `X_PLEX_TOKEN`, `X-Plex-Token`, any `PLEX_PG_*`, or `apikey=`.
- Fixtures say `REDACTED`.
- A capture of a process environment keeps only `EAE_ROOT`, `FFMPEG_EXTERNAL_LIBS` and `X_PLEX_TOKEN`, the last as `REDACTED`.

**clustarr.**
- The manager only reads the Lease and stays the sole writer of clustarr's resources.
- The new flag must not start with `--livetv-`.
- Gates: `go build ./...`, the named unit tests, `make manifests` leaving `config/` unchanged, `make brand-gate`, and `helm lint --strict charts/clustarr`. No envtests, which get one final run per the owner.

**Licences.** cluster-plex does not import clustarr, which is GPL-3.0. clustarr's QSV code (`pkg/transcode/engine/video.go`, `pkg/transcode/standard/plan.go:455-470`, `pkg/transcode/selfcheck/trial.go`) is a reference to read, not code to vendor.

**Deploying.**
- talos is deployed only through cluster-bootstrap's Terraform path. Never `helm upgrade`.
- Announce a deploy to peer sessions first.
- Restart Plex only when `clusterplex_plex_sessions` is 0.
- Compare rendered manifests as parsed YAML.

**Gates per repo.**
- cluster-plex: `make test`, `make test-native` (Task 2), `make lint`, `make helm-lint`.
- ffgo: `go test ./...` with `FFGO_SHIM_DIR` set to a freshly built shim.

## Review Focus

1. **Burn-in of a file whose path holds `'`, `:` or `,`.** For example `Schindler's List (1993)` or `Mission: Impossible`. The path goes inside a `subtitles=filename=…` filter argument, so it must be escaped, not mangled, and the job must still burn in. Pinned in Task 12 by `TestAPathWithAQuoteAndAColonBurnsIn`.
2. **A seek on a full pod.** Plex kills the old job and starts a new one for the same session, often before the old slot is released. The restart must take its own session's slot, not be refused `full`. Pinned in Task 9 by `TestARestartTakesItsSessionsSlot`.
3. **A Live TV upstream that stalls.** The relay stops sending input bytes, but there is no EOF. The job must fail `worker-error` and release its slot and its Lease count within the stall timeout, and never hang a slot. Pinned in Task 14 by `TestAStalledStreamedInputReleasesItsSlot`.
4. **A fourth stream, or a download, while 3 slots are busy or 2 are.** A fourth stream is refused `full` at once. A whole-file job never takes the last free slot. Pinned in Task 8 by `TestAFullPoolIsRefusedFull` and in Task 9 by `TestSlotsHoldOneBackForStreams`.
5. **An irregular frame rate in the argv** (`-r:0 23.975999999999999`). Intel's runtime refuses irregular fractions; it failed clustarr's `hevc_qsv` with -38 on 2026-10-20. The parser must give the encoder `24000/1001`, and the output must be constant-frame-rate at that rate. Pinned in Task 4 by `TestAnIrregularFrameRateIsPlexsRate` and in Task 10 by `TestTheOutputIsWhatPlexAskedFor`.

## Findings this plan depends on

Task 1 records each finding, under the letter below, in the spec's new "As captured" section and in fixtures. Later tasks cite the letter. Where a finding chooses between named alternatives, the task says what each alternative means for it.

- **L, the segment list.**
  - How the transcoder sends it: one POST per line, or one long request (`X-Plex-Http-Pipeline=infinite`).
  - The exact CSV line, including what `-segment_list_separate_stream_times 1` adds.
  - Fixture: `pkg/video/testdata/segment-lists/<shape>.txt`.
- **R, the cut rule.** Where segments start for a job from 0 and for a restart with `-ss` and `-segment_start_number`, read from the times in L's lines.
- **V, progress.** The paths and query keys a video conversion sends under `-progressurl`. Fixture: `pkg/video/testdata/progress/<shape>.txt`.
- **P, throttling.** How PMS paces its transcoder. The alternatives:
  - `stopped`: PMS's child shows state `T` in `/proc/<pid>/stat` while it runs ahead;
  - `unread`: PMS stops reading the segment-list request;
  - `answer`: what PMS's responses to progress or list requests say;
  - plus the PMS log lines matching `(?i)throttl|sloth`.
- **I, Live TV input.** The `-i` URL, the method and headers the transcoder uses on it, and the demuxer it names.
- **B, burn-in.** The argv for a text sidecar, an embedded text track and an image (PGS) track.
- **H and D.** The argv for HDR10 to SDR, and for an interlaced source.
- **W, DASH with encode.** The argv for Plex Web converting video into DASH.
- **F, whole-file jobs.** For an iOS download, an Optimize and a DVR convert:
  - the output path;
  - the container and muxer options, including whether any asks for `+faststart`;
  - the progress keys.

---

### Task 0: Amend the spec

**Files:**
- Modify: `docs/superpowers/specs/2026-10-26-gpu-video-transcode-pool-design.md`

- [ ] **Step 1: Replace the status line**

Replace `**Status:** draft for the owner's review (2026-10-26)` with:
```
**Status:** approved by the owner (2026-10-26); amended the same day (owner): the worker writes nothing to disk
```

- [ ] **Step 2: Add the no-disk rule to §3**

Insert after the `**The pipeline**, per job, …` list (before `**Concurrency:**`):
```
**Nothing on disk (owner, 2026-10-26).** The worker muxes every output in
memory and streams it back: an HLS or DASH segment whole, a whole-file job's
bytes as (offset, data) writes the relay applies to the output file. No
FFmpeg muxer writes a file on the NUC and there is no temporary output file.
The one file the worker holds is a subtitle the manager streams for burn-in,
in a memory-backed emptyDir, because FFmpeg's `subtitles` filter reads a file.
```

- [ ] **Step 3: Record the calls the plan made**

Replace the `- **How to segment:** …` bullet under "Left to the builder" with:
```
- **How to segment** (decided in the plan): one MPEG-TS muxer per job
  writing into memory, drained and cut at each forced keyframe the cut rule
  names; ffgo's HLS segmenter is not used, since it writes files.
- **Image subtitles** (decided in the plan): composited in Go onto the CPU
  frame from ffgo's decoded bitmaps; ffgo's filter graph is one linear chain,
  so FFmpeg's two-input `overlay` is not available.
```
Append to §6 "The signal, written by cluster-plex", after the renew paragraph:
```
Each manager keeps its own count in `clusterplex.io/video-transcodes.<pod>:
"<n> <unix seconds>"` and writes the annotation as the sum of the entries
renewed within 30 s, so one manager's last conversion ending cannot zero
another's (the plan's call; one manager is the talos case). clustarr's flag
is `--video-transcode-lease <namespace>/<name>`, chart value
`manager.videoTranscodeLease`, empty for no yield. clustarr's manager role
already grants get, list and watch on leases (for Live TV's), so the yield
adds no grant; a ClusterRole generated from markers cannot name one Lease.
```

- [ ] **Step 4: Commit**

```bash
git commit -m 'docs(spec): GPU pool -- the worker writes nothing to disk (owner), and the calls the plan made' -- docs/superpowers/specs/2026-10-26-gpu-video-transcode-pool-design.md
```

---

### Task 1: Capture Plex's own conversions

Plex still converts video on the Pi today (f73c548), which makes this the moment to record its real transcoder. Several shapes need the owner to act in an app; ask for each in one message, in the table's order.

**Files:**
- Create: `hack/capture-video-jobs.sh`, `hack/capture-video-jobs.inner.sh`
- Create: `pkg/video/testdata/plex-video-jobs.jsonl`, `pkg/video/testdata/segment-lists/<shape>.txt`, `pkg/video/testdata/progress/<shape>.txt`
- Create: `pkg/video/fixtures_test.go`
- Modify: the spec, adding a final `## As captured (<date>)` section with findings L, R, V, P, I, B, H, D, W, F

**Interfaces:**
- Produces: the fixture formats.
  - `plex-video-jobs.jsonl` has one JSON object per line: `{"shape": string, "args": [string] /* without argv[0] */, "env": {"EAE_ROOT"?, "FFMPEG_EXTERNAL_LIBS"?, "X_PLEX_TOKEN": "REDACTED"}, "cwd": string}`.
  - `segment-lists/<shape>.txt` is the lines in the order sent.
  - `progress/<shape>.txt` has one `path?query` per line.
  - Shapes are named by the table below.

- [ ] **Step 1: Write the secrets guard, and watch it fail**

`pkg/video/fixtures_test.go` declares `package video`. Write `TestFixturesCarryNoSecrets`: it walks `pkg/video/testdata` and `../remux/testdata`, and fails naming the file and line on any of these:
- `X_PLEX_TOKEN` or `X-Plex-Token` with a value other than `REDACTED`;
- a `PLEX_PG_` key;
- `apikey=` or `api_key=` with a non-empty value;
- `"password"`.

Write a throwaway `testdata/plex-video-jobs.jsonl` containing `{"env":{"X_PLEX_TOKEN":"abc123"}}`.
Run: `go test ./pkg/video/ -run TestFixturesCarryNoSecrets -v`
Expected: FAIL naming `plex-video-jobs.jsonl:1`. Delete the line.

- [ ] **Step 2: Write the capture script**

`hack/capture-video-jobs.sh <seconds>` runs the inner script on `plex-0`:
```
kubectl debug plex-0 -q -i --image=nicolaka/netshoot:v0.13 --target=plex --profile=general --custom=<root-debug.json> -- sh -s < hack/capture-video-jobs.inner.sh
```
- `root-debug.json` is `{"securityContext":{"runAsUser":0,"runAsGroup":0,"runAsNonRoot":false,"allowPrivilegeEscalation":false}}`.
- Afterwards the host script waits for `<seconds>`, then saves `kubectl logs` of the newest ephemeral container (sort `ephemeralContainerStatuses` by `startedAt`; `kubectl debug -i` returns early) to the scratchpad.

The inner script runs for `<seconds>` and does three things:
- **Find PMS.** PMS is the process whose cmdline is exactly `/usr/lib/plexmediaserver/Plex Media Server`. That is pid 19; pid 18 is the subreaper, whose cmdline also ends in that name.
- **Watch transcoders,** every 0.5 s, for each process whose argv[0] ends in `Plex Transcoder`:
  - On first sight it prints one `JOB` JSON line: args, cwd, ppid, `pms_child: ppid==PMS`, and only the three allowed env keys, the token as `REDACTED`. It never reads any other env value into a variable.
  - On each sample it prints a `STATE pid letter time` line when field 3 of `/proc/<pid>/stat` changes.
- **Record the wire.** `tcpdump -i any -s0 -A -l 'tcp port 32400'` piped through
  ```
  sed -E 's/(X-Plex-Token[=:] ?)[A-Za-z0-9_-]+/\1REDACTED/g'
  ```
  writing `HTTP` lines to the same output.
- At the end it greps the PMS log in the config volume for `(?i)throttl|sloth` over the run's minutes.

- [ ] **Step 3: Run the captures**

| shape | how | notes |
|---|---|---|
| `library-hls` | API: start a transcode as Plex for iOS. Reuse the scratchpad's `livetv/capture.sh` request and identity headers. Hold the session for 4 min, past `TranscoderThrottleBuffer` | P, L, V |
| `library-hls-seek` | the same session, restarted at `offset=1800` | R |
| `livetv-hls` | the owner tunes a channel in Plex for iOS for 2 min | I |
| `livetv-interlaced` | the owner tunes a 1080i or MPEG-2 channel, if the lineup has one. Otherwise record "none in the lineup" and capture a library MPEG-2 interlaced file, if any | D |
| `burn-text-sidecar`, `burn-text-embedded`, `burn-image` | the owner plays a film with that subtitle in Plex for iOS, with burn subtitles set to "Always" | B |
| `hdr-sdr` | an HDR10 film in Plex Web in Firefox at 4 Mbps | H |
| `dash-encode` | an HEVC film in Plex Web at 4 Mbps | W |
| `download` | the owner downloads a film at a converted quality in Plex for iOS | F |
| `optimize` | the owner optimizes one short episode for mobile. This writes a Plex Version into the library: **ask first, and delete it afterwards with the owner's OK** | F |
| `dvr-convert` | the owner turns on "Convert video while recording", records one minute, then turns it off. This changes a Plex setting and writes a recording: **ask first, and delete the recording afterwards with the owner's OK** | F |

- [ ] **Step 4: Extract the fixtures and write the findings**

- Write one `plex-video-jobs.jsonl` line per shape.
- Copy the segment-list bodies and progress requests from the redacted `HTTP` lines into their files.
- Write the "As captured" section, one paragraph per letter, with the evidence each rests on (line counts, `STATE` transitions, PMS log lines). For P, name which alternative holds.

- [ ] **Step 5: Run the guard**

Run: `go test ./pkg/video/ -run TestFixturesCarryNoSecrets -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add hack/capture-video-jobs.sh hack/capture-video-jobs.inner.sh pkg/video/fixtures_test.go pkg/video/testdata
git commit -m 'video: Plex'\''s own conversions, captured -- argv, segment lists, progress and throttling for every shape the pool serves, redacted' -- hack/capture-video-jobs.sh hack/capture-video-jobs.inner.sh pkg/video docs/superpowers/specs/2026-10-26-gpu-video-transcode-pool-design.md
```

---

### Task 2: ffgo .13 published, and cluster-plex on it

**Files:**
- Modify: `go.mod:118` and `go.sum`
- Modify: `cmd/remux-worker/main.go`, `Makefile`, and `pkg/remux/pipeline/pipeline_test.go` (the skip helper)
- Test: `cmd/remux-worker/main_test.go`

**Interfaces:**
- Produces:
  - `func checkRuntime(version func() (avutil, avcodec, avformat uint32), shimAPI func() int, minShim int) error` in `cmd/remux-worker`, which `cmd/video-worker` copies.
  - `make test-native`.
  - The env `CLUSTERPLEX_REQUIRE_FFGO=1`: under it a test that would skip for missing FFmpeg 9 or a missing shim fails instead.

- [ ] **Step 1: Push the tag**

Run:
```
git -C /home/appkins/src/mediactl/ffgo-unify remote get-url origin
git -C /home/appkins/src/mediactl/ffgo-unify push origin v0.0.0-clustarr.13
```
Expected: the origin is `github.com/mediactl/ffgo`, and the push prints `* [new tag] v0.0.0-clustarr.13 -> v0.0.0-clustarr.13`. Then run `go list -m github.com/mediactl/ffgo@v0.0.0-clustarr.13`, retrying for up to 5 minutes, until it prints the version.

- [ ] **Step 2: Write the failing test**

Write `TestTheWorkerRefusesAnOldShim` with these cases:
- `checkRuntime(v63, func() int { return 0 }, 1)` returns an error naming the shim API;
- the same with `1` returns nil;
- an avcodec major of 62 returns an error naming libavcodec.

Run: `go test ./cmd/remux-worker/ -run TestTheWorkerRefusesAnOldShim -v`
Expected: FAIL, `checkRuntime` undefined.

- [ ] **Step 3: Move the pin and add `checkRuntime`**

- Edit the `replace` line to `v0.0.0-clustarr.13`.
- Run `go mod download github.com/mediactl/ffgo@v0.0.0-clustarr.13`, then `go build ./...`.
- Move the start-up check in `cmd/remux-worker/main.go` into `checkRuntime(ffgo.Version, ffgo.ShimAPIVersion, 1)`.

- [ ] **Step 4: Add `make test-native`**

Add `bin/libffshim.so`, built by the same `gcc` line that `images/Dockerfile.remux`'s ffmpeg stage uses:
- the source is `$(go list -m -f '{{.Dir}}' github.com/obinnaokechukwu/ffgo)/shim`;
- it builds against the system FFmpeg 9 through `pkg-config --cflags --libs libavformat libavcodec libavfilter libavutil libswresample libswscale`;
- its prerequisite is `go.mod`.

Then add:
```make
test-native: bin/libffshim.so ## The ffgo suites against the system FFmpeg 9; they fail rather than skip
	FFGO_SHIM_DIR=$(CURDIR)/bin CLUSTERPLEX_REQUIRE_FFGO=1 go test ./pkg/remux/pipeline/... ./pkg/video/...
```
In `pipeline_test.go`'s `clip`, call `t.Fatal` instead of `t.Skip` when `CLUSTERPLEX_REQUIRE_FFGO=1`.

- [ ] **Step 5: Run the gates**

Run: `go test ./cmd/remux-worker/ -v && make test-native && make test && make lint`
Expected: everything passes, and `pkg/remux/pipeline`'s tests run rather than skip.

- [ ] **Step 6: Commit**

```bash
git commit -m 'build: ffgo v0.0.0-clustarr.13 -- the hardware decode and interrupt work clustarr built; the remux worker refuses a shim older than API 1, and make test-native runs the ffgo suites for real' -- go.mod go.sum Makefile cmd/remux-worker pkg/remux/pipeline/pipeline_test.go
```

---

### Task 3: ffgo extensions, tag .14, and cluster-plex on it

**Files** (ffgo-unify, branch `clustarr/unify-media`):
- Modify: the `Frame` type's file (`ffgo.go`), `muxer.go`, `io.go`, and the subtitle decoder's file (find it with `grep -n "type SubtitleDecoder" *.go`)
- Modify: `shim/ffshim.h`/`.c` only if a field needs a new accessor. If so, set `FFSHIM_API_VERSION 2` with the comment `2 = v0.0.0-clustarr.14`.
- Test: `clusterplex_test.go`

**Interfaces:**
- Produces:
  ```go
  func (f Frame) SetPictureType(t avutil.PictureType)                 // AV_PICTURE_TYPE_I forces a keyframe; None clears
  func (m *Muxer) Drain() error                                       // av_interleaved_write_frame(NULL), av_write_frame(NULL), avio_flush: every byte of every packet written so far is in the writer
  func NewMuxerToWriteSeeker(w io.WriteSeeker, format string) (*Muxer, error) // an AVIO context with write and seek callbacks, as io.go's IOCallbacks gives decoders
  type SubtitleBitmap struct { X, Y, W, H, Stride int; Pixels []byte /* PAL8 */; Palette []uint32 /* ARGB */ }
  type Subtitle struct { Start, End time.Duration; Bitmaps []SubtitleBitmap; Text []string; CanvasW, CanvasH int }
  func (d *Decoder) NewSubtitleStreamDecoder(streamIndex int) (*SubtitleStreamDecoder, error)
  func (s *SubtitleStreamDecoder) Decode(p *Packet) (*Subtitle, error) // nil, nil: no event yet
  func (s *SubtitleStreamDecoder) Close() error
  ```
- If an existing packet-level subtitle decoder already covers part of this, extend it rather than add a second, and record its name in the ledger. cluster-plex uses only the names above.

- [ ] **Step 1: Write the four failing tests**

- **`TestAForcedFrameEncodesAsAKeyframe`.** Encode 30 frames from a `testsrc2` lavfi decoder with libx264, GOPSize 250, calling `SetPictureType(avutil.PictureTypeI)` on frame 12. Only packets 0 and 12 carry the key flag.
- **`TestDrainHandsEveryWrittenPacketToTheWriter`.** An mpegts muxer writes into a `bytes.Buffer`, with 50 video packets interleaved with audio, then `Drain()`. Decoding a copy of the buffer through `NewDecoderFromReader(…, "mpegts")` yields all 50 video packets' pts. Write 50 more, `Drain()`, and all 100 decode.
- **`TestAnMP4MuxedThroughAWriteSeekerPlays`.** Mux 60 frames into a non-fragmented mp4 through an in-memory `io.WriteSeeker` (a test type over a `[]byte`). The bytes contain a `moov` box, and decoding them yields 60 frames at the written duration.
- **`TestAPGSSubtitleDecodesToItsBitmap`.** A test helper `writeSup(path string, x, y, w, h int, argb uint32, start, end time.Duration)` writes one PGS display set: `PG` magic; PCS, WDS, PDS, ODS (RLE) and END segments; 90 kHz times. Decoding it gives one `Subtitle` with one bitmap at (x, y, w, h), whose palette lookup of every pixel is `argb`, spanning start to end.

Run: `go test ./ -run 'TestAForcedFrame|TestDrain|TestAnMP4Muxed|TestAPGS' -v`
Expected: FAIL, undefined.

- [ ] **Step 2: Implement the five functions and the bitmap types**

- `SetPictureType` writes `pict_type` through the same shim accessor `internal/shim/hwframes.go:35` uses.
- `Drain` calls `avformat.InterleavedWriteFrame(ctx, 0)`, then `avformat.WriteFrame(ctx, 0)`, then `avio_flush`.
- The subtitle decoder copies `AVSubtitleRect`s of type `SUBTITLE_BITMAP` into Go memory before `avsubtitle_free`.

- [ ] **Step 3: Run the whole suite**

Run: `FFGO_SHIM_DIR=<a shim built from this tree> go test ./...`
Expected: PASS, including the four new tests.

- [ ] **Step 4: Commit, tag and push the tag**

```bash
git -C /home/appkins/src/mediactl/ffgo-unify commit -m 'clusterplex: forced keyframes, Drain, a muxer over an io.WriteSeeker, and bitmap subtitles -- what cluster-plex'\''s GPU pool needs to segment in memory and burn image subtitles in' -- ffgo.go muxer.go io.go clusterplex_test.go <the subtitle decoder'\''s file> shim
git -C /home/appkins/src/mediactl/ffgo-unify tag -a v0.0.0-clustarr.14 -m 'clusterplex GPU pool extensions'
git -C /home/appkins/src/mediactl/ffgo-unify push origin v0.0.0-clustarr.14
```

- [ ] **Step 5: Move cluster-plex to .14**

Repeat Task 2's Step 3 with `.14`. If the shim API moved to 2, make `checkRuntime`'s minimum 2 and add a case to `TestTheWorkerRefusesAnOldShim`.
Run: `make test-native && make test && make lint`
Expected: PASS.

```bash
git commit -m 'build: ffgo v0.0.0-clustarr.14 -- forced keyframes, Drain, write-seek muxing and bitmap subtitles for the GPU pool' -- go.mod go.sum cmd/remux-worker
```

---

### Task 4: `pkg/video`, the job and its parser

**Files:**
- Create: `pkg/video/job.go`, `pkg/video/parse.go`, `pkg/video/classify.go`
- Test: `pkg/video/parse_test.go`, `pkg/video/classify_test.go`

**Interfaces:**
- Produces (pure Go, no ffgo; the manager imports it):
  ```go
  var ErrShape = errors.New("not a video conversion the pool runs")
  type Reason string
  const (
  	ReasonNoPool      Reason = "no-pool"
  	ReasonFull        Reason = "full"
  	ReasonShape       Reason = "shape"
  	ReasonWorkerError Reason = "worker-error"
  	ReasonCancelled   Reason = "cancelled"
  )
  type OutputKind string
  const (OutputHLS OutputKind = "hls"; OutputDASH OutputKind = "dash"; OutputFile OutputKind = "file")
  type Rational struct{ Num, Den int }
  type Input struct {
  	Path     string // under /media: the worker reads it from its own mount
  	Streamed bool   // anything else: the manager streams it
  	Format   string // a streamed input's demuxer
  	Name     string // a streamed file's base name
  	Source   string // the manager's alone, never on the wire: the URL or local path it streams
  }
  type VideoOut struct {
  	Stream, Width, Height, CRF int
  	PixFmt                    string
  	MaxRate, BufSize          int64
  	FrameRate                 Rational
  	KeyframeEvery             time.Duration
  	Deinterlace, ToneMap      bool
  }
  type AudioOut struct { Stream int; Copy bool; Channels, SampleRate int; BitRate int64; Language string }
  type BurnIn struct { Input, Stream int; Image bool }
  type Segmenting struct { Duration time.Duration; StartNumber int; Pattern string; ListSize int; TimeDelta time.Duration }
  type FileOut struct { Format string; MuxerOptions map[string]string }
  type Job struct {
  	Output   OutputKind
  	Session  string // from -progressurl's /session/<s>/
  	Inputs   []Input
  	Start    time.Duration
  	Video    VideoOut
  	Audio    *AudioOut
  	Burn     *BurnIn
  	Segments Segmenting
  	File     FileOut
  	// The manager's alone; never on the wire.
  	Token, ProgressURL, SegmentListURL, ManifestURL, OutputPath string
  }
  func Parse(args []string, env map[string]string) (Job, error) // errors wrap ErrShape
  func IsConversion(args []string) bool
  func (j Job) RingKey() string // Inputs[0].Path, else Session
  ```

- [ ] **Step 1: Write the failing tests**

**`TestTheLibraryHLSJobParses`** parses the `library-hls` fixture and asserts:
```go
require.Equal(t, video.OutputHLS, j.Output)
require.False(t, j.Inputs[0].Streamed)
require.True(t, strings.HasPrefix(j.Inputs[0].Path, "/media/movies/Grizzly Man (2005) {tmdb-501}/"))
require.Equal(t, video.VideoOut{Stream: 0, Width: 720, Height: 404, PixFmt: "nv12", CRF: 17,
	MaxRate: 1_818_000, BufSize: 3_636_000, FrameRate: video.Rational{24000, 1001},
	KeyframeEvery: 8 * time.Second}, j.Video)
require.Equal(t, &video.AudioOut{Stream: 2, Channels: 2, SampleRate: 96000, BitRate: 232_000, Language: "eng"}, j.Audio)
require.Equal(t, video.Segmenting{Duration: 8 * time.Second, Pattern: "media-%05d.ts", ListSize: 5,
	TimeDelta: 62500 * time.Microsecond}, j.Segments)
require.Equal(t, "REDACTED", j.Token)
require.Contains(t, j.SegmentListURL, "/manifest?X-Plex-Http-Pipeline=infinite")
```
Two notes on these values:
- `PixFmt` is `nv12`: Plex allows `yuv420p|nv12`, and the bitstream is the same.
- The capture redacted the job's `-progressurl`'s `<session>`. Use whatever placeholder the fixture carries.

**`TestEveryCapturedVideoJobParses`:** every fixture line parses, and its `Output` matches its shape. `dash-encode` is DASH; `download`, `optimize` and `dvr-convert` are File; the rest are HLS. The `livetv-*` shapes have `Inputs[0].Streamed`, per finding I.

**`TestAnUnknownVideoOptionIsRefused`** is table-driven over the library argv, with these edits:
- `-ac 6` inserted before the output;
- `libx264` swapped for `libx265`;
- `drawtext=text=x` appended to the video chain;
- `-f flv`;
- `-vf yadif`;
- `-segment_format mp4`.

Each case satisfies `errors.Is(err, video.ErrShape)`, and its message names the option or filter.

**`TestOnlyAVideoConversionIsOne`:**
- false for every line of `pkg/remux/testdata/plex-dash-jobs.log`;
- true for every conversion fixture;
- false for the library argv with `-codec:0 copy` and its video `-filter_complex` removed (an audio-only conversion);
- true for `-codec:0 foo`. An unknown encoder is a conversion, so it is refused rather than run on the Pi. Ledger this as a ruling.

**`TestAnIrregularFrameRateIsPlexsRate`:** `23.975999999999999` gives {24000,1001}; `29.97002997` gives {30000,1001}; `59.94005994` gives {60000,1001}; `25` gives {25,1}; `12.5` gives {25,2}.

Run: `go test ./pkg/video/ -v`
Expected: FAIL, undefined.

- [ ] **Step 2: Implement the parser and the classifier**

The structure follows `pkg/remux/parse.go`:
- `valueOptions` and `flagOptions` allow-lists with per-output keys (`-codec:0`, `-crf:0`, `-maxrate:0`, `-bufsize:0`, `-r:0`, `-preset:0`, `-x264opts:0`, `-force_key_frames:0`, `-b:1`, `-metadata:s:1`), seeded from every captured argv;
- `refuse()` wraps `ErrShape`;
- `-codec:N` before `-i` is a decoder hint, read and ignored.

Filter chains:
- Each `-filter_complex` is parsed into `[in]name=k=v:k=v,name[out]` chains with their labels resolved.
- The video filters accepted are `scale` (w, h, force_divisible_by) and `format` (pix_fmts), plus those findings B, H and D add. Each is mapped onto a typed field: Width/Height, Deinterlace, ToneMap, Burn.
- The audio filter accepted is `aresample` (async, ochl, rematrix_maxval, osr).

Codecs and outputs:
- `h264` is the only video output codec accepted (`libx264`, or as captured).
- `-force_key_frames:0` accepts only `expr:gte(t,n_forced*N)`.
- The output is the last argument. `-f ssegment` with `-segment_format mpegts` is HLS; `-f dash` with an encoded video is DASH; finding F's muxers are File.
- `IsConversion` is true iff some output codec is neither `copy` nor in the audio and subtitle encoder set: `aac`, `ac3`, `eac3`, `mp3`, `libopus`, `opus`, `flac`, `pcm_*`, `srt`, `ass`, `mov_text`, `webvtt`.
- Frame rates within 1e-4 relative of `N*1000/1001`, for N in {24, 30, 48, 60, 120}, take that rational. Any other rate is milli-rounded and reduced.

- [ ] **Step 3: Run the tests**

Run: `go test ./pkg/video/ -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add pkg/video/job.go pkg/video/parse.go pkg/video/classify.go pkg/video/parse_test.go pkg/video/classify_test.go
git commit -m 'video: the job and its parser -- an allow-list built from captured Plex conversions; anything else is refused by name' -- pkg/video
```

---

### Task 5: Segment rules, the proto and the wire

**Files:**
- Create: `pkg/video/segment.go`, `pkg/video/wire.go`, `proto/video.proto`, and the generated `proto/videopb/video.pb.go` and `video_grpc.pb.go`
- Modify: `Makefile` (the `proto` target)
- Test: `pkg/video/segment_test.go`, `pkg/video/wire_test.go`

**Interfaces:**
- Produces:
  ```go
  func KeyframeDue(t time.Duration, forced int, every time.Duration) bool // FFmpeg's gte(t, n_forced*every)
  func CutDue(pts, origin time.Duration, cut int, s Segmenting) bool      // finding R's rule; origin as R defines it
  func SegmentName(pattern string, n int) string
  type Span struct { Stream int; Start, End time.Duration }
  type SegmentTimes struct { Name string; Start, End time.Duration; Streams []Span }
  func SegmentListLine(s SegmentTimes) string                            // finding L's line, newline included
  func (j Job) Proto() *videopb.Job
  func JobFromProto(p *videopb.Job) Job
  ```
- The proto is written exactly as below.

```proto
syntax = "proto3";

package video;

option go_package = "github.com/mediactl/clusterplex/proto/videopb";

// Video runs one of Plex's video conversions on the GPU pool. The first Up
// is the Job; every later Up is a streamed input's bytes. Every Down is the
// job's output in order, the last a Done. A worker with no free slot answers
// RESOURCE_EXHAUSTED before any Down; a job it cannot run, INVALID_ARGUMENT.
service Video {
  rpc Convert(stream Up) returns (stream Down);
}

message Up {
  oneof kind {
    Job job = 1;
    Chunk input = 2;
  }
}

// Chunk is the next bytes of Job.inputs[input]; eof ends it, error ends it
// failed (the manager lost its source).
message Chunk {
  int32 input = 1;
  bytes data = 2;
  bool eof = 3;
  string error = 4;
}

message Job {
  string output = 1;           // "hls", "dash" or "file"
  string session = 2;
  repeated Input inputs = 3;   // [0] the media
  int64 start_us = 4;
  Video video = 5;
  Audio audio = 6;             // unset: no audio output
  BurnIn burn = 7;             // unset: none
  Segmenting segments = 8;
  FileOut file = 9;
}

message Input {
  string path = 1;    // under /media; empty when streamed
  bool streamed = 2;
  string format = 3;  // a streamed input's demuxer, e.g. "mpegts"
  string name = 4;    // a streamed file's base name, e.g. "sub.srt"
}

message Video {
  int32 stream = 1;
  int32 width = 2;
  int32 height = 3;
  string pix_fmt = 4;
  int32 crf = 5;
  int64 max_rate = 6;
  int64 buf_size = 7;
  int32 rate_num = 8;
  int32 rate_den = 9;
  int64 keyframe_every_us = 10;
  bool deinterlace = 11;
  bool tone_map = 12;
}

message Audio {
  int32 stream = 1;
  bool copy = 2;
  int32 channels = 3;
  int32 sample_rate = 4;
  int64 bit_rate = 5;
  string language = 6;
}

message BurnIn {
  int32 input = 1;
  int32 stream = 2;
  bool image = 3;
}

message Segmenting {
  int64 duration_us = 1;
  int32 start_number = 2;
  string pattern = 3;
  int32 list_size = 4;
  int64 time_delta_us = 5;
}

message FileOut {
  string format = 1;
  map<string, string> muxer_options = 2;
}

message Down {
  oneof kind {
    Segment segment = 1;
    File file = 2;
    Manifest manifest = 3;
    Progress progress = 4;
    Write write = 5;
    Done done = 6;
  }
}

// Segment is one whole HLS segment and the times its list line reports.
message Segment {
  string name = 1;
  bytes data = 2;
  int64 start_us = 3;
  int64 end_us = 4;
  repeated Span streams = 5;
}

message Span {
  int32 stream = 1;
  int64 start_us = 2;
  int64 end_us = 3;
}

message File {
  string name = 1;     // a DASH init or chunk
  bytes data = 2;
}

message Manifest {
  bytes mpd = 1;
}

message Progress {
  string path = 1;
  string query = 2;
}

message Write {
  int64 offset = 1;    // a whole-file output's bytes at offset
  bytes data = 2;
}

message Done {
  string error = 1;    // empty: success
  string reason = 2;   // a video.Reason when error is set
}
```

- [ ] **Step 1: Write the failing tests**

**`TestKeyframesAreForcedAsPlexsExpressionSays`:**
- `KeyframeDue(0, 0, 8s)` is true;
- `(40ms, 1, 8s)` is false;
- `(7999ms, 1, 8s)` is false;
- `(8s, 1, 8s)` is true;
- `(1800s, 0, 8s)` is true.

**`TestSegmentNamesFollowThePattern`:** `media-00007.ts` for 7, and `media-123456.ts` for 123456.

**`TestCutsFallWhereFindingRSays`:** for `library-hls` and `library-hls-seek`, feed `CutDue` the keyframe times implied by each captured line's start. It cuts exactly at the captured boundaries and nowhere else.

**`TestSegmentListLinesArePlexs`:** `SegmentListLine` of each captured segment's times reproduces `segment-lists/library-hls.txt` byte for byte.

**`TestAJobCrossesTheWireWithoutSecrets`:**
- `JobFromProto(j.Proto())` equals `j` with Token, ProgressURL, SegmentListURL, ManifestURL, OutputPath and every `Input.Source` zeroed, for every fixture.
- `proto.Marshal(j.Proto())` contains none of those strings.

Run: `go test ./pkg/video/ -v`
Expected: FAIL, undefined.

- [ ] **Step 2: Generate the bindings**

Add to the Makefile's `proto` target:
```
mkdir -p proto/videopb
protoc -I proto --go_out=proto/videopb --go_opt=paths=source_relative --go-grpc_out=proto/videopb --go-grpc_opt=paths=source_relative proto/video.proto
```
Run `protoc --version`, `protoc-gen-go --version` and `protoc-gen-go-grpc --version`. Expected: v7.36.0, v1.36.12 and v1.6.2, the versions `proto/remuxpb` records. Install those versions if they differ. Then run `make proto`.

- [ ] **Step 3: Implement the segment rules and the wire**

`SegmentListLine` and `CutDue` follow findings L and R. Ledger the rule R names as it stands in the spec.

- [ ] **Step 4: Run the tests**

Run: `go test ./pkg/video/ -v && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add proto/video.proto proto/videopb pkg/video/segment.go pkg/video/wire.go pkg/video/segment_test.go pkg/video/wire_test.go
git commit -m 'video: the Video service, and Plex'\''s segment rules -- keyframes forced as its expression says, cuts and list lines as its transcoder made them' -- proto Makefile pkg/video
```

---

### Task 6: The session helpers, and the dispatcher's no-fallback guard

**Files:**
- Create: `pkg/plexsession/session.go`, `pkg/plexsession/session_test.go`
- Modify: `pkg/remux/relay/relay.go` (use the helpers; remove its own `finish`, `writeFile` and `send`), `pkg/remoteexec/dispatcher.go`, `pkg/telemetry/telemetry.go`
- Test: `pkg/remoteexec/dispatcher_test.go`

**Interfaces:**
- Consumes: `video.IsConversion`, `video.Reason` (Task 4).
- Produces:
  ```go
  // pkg/plexsession
  func WriteFile(dir, name string, data []byte, allowed *regexp.Regexp) error // .<name>.tmp, then rename
  type PMS struct { Addr string /* "http://host:port" */; HTTP *http.Client }
  func (p PMS) Send(ctx context.Context, method, raw, sub, query string, body io.Reader, token string) error
  func (p PMS) Open(ctx context.Context, raw, token string) (io.ReadCloser, error) // GET at Plex's own address
  func Finish(sink remoteexec.Sink, prefix string, err error) error              // "<prefix>: <err>\n" on stderr, exit 1; nil: exit 0
  // pkg/remoteexec
  type VideoRoute interface { Execute(ctx context.Context, req *pb.ExecRequest, sink Sink) error }
  // Dispatcher gains: Video VideoRoute
  // pkg/telemetry
  VideoRefusals *prometheus.CounterVec // clusterplex_video_refusals_total{reason}
  ```

- [ ] **Step 1: Write the failing tests**

- **`TestWriteFileRefusesANameOutsideThePattern`:** `../x.ts`, `x.ts` and `media-1.ts` are refused against `^media-\d{5,}\.ts$`, and nothing is written.
- **`TestPMSSendRewritesToPlexsOwnAddress`:** a `127.0.0.1:32400` URL reaches the test server at `Addr`, carrying `X-Plex-Token`.
- **`TestAVideoConversionNeverRunsLocally`:** a `Dispatcher` with a recording `Local`, a recording `Workers` and `Video: nil`, given the `library-hls` request:
  - the sink receives stderr `video: no-pool: no video transcode pool is configured\n` and exit 1;
  - `Local` never ran and `Workers` was never listed;
  - `VideoRefusals{reason="no-pool"}` is 1.
- **`TestAVideoConversionTakesTheVideoRoute`:** the fake `Video` is called once, `Local` is not, and `OnRoute` receives `("Plex Transcoder", "video")`.
- **`TestABrowserRemuxStillTakesTheRemuxPool`:** a `plex-dash-jobs.log` line goes to `Remux`, not `Video`.
- **`TestAnAudioOnlyConversionStillRunsWhereItDid`:** the audio-only argv from Task 4 runs on `Local`.

Run: `go test ./pkg/plexsession/ ./pkg/remoteexec/ -v`
Expected: FAIL.

- [ ] **Step 2: Implement**

- Move the remux relay's helpers into `pkg/plexsession`; the remux relay calls `Finish(sink, "remux", err)`.
- In `Dispatcher.Execute`, after the `Remux` step: if the target is `Transcoder` and `video.IsConversion(req.GetArgs())`, then call `d.Video.Execute` when `Video` is set, or refuse `no-pool`, and return in both cases.

- [ ] **Step 3: Run the tests**

Run: `go test ./pkg/plexsession/ ./pkg/remoteexec/ ./pkg/remux/... -v`
Expected: PASS, with the remux relay's existing tests unchanged.

- [ ] **Step 4: Commit**

```bash
git add pkg/plexsession
git commit -m 'remoteexec: a video conversion never runs on the Pi -- the Video route takes it or it is refused, no-pool; the relays share their session helpers' -- pkg/plexsession pkg/remoteexec pkg/remux/relay pkg/telemetry
```

---

### Task 7: The Lease counter

**Files:**
- Create: `pkg/video/lease/lease.go`, `pkg/video/lease/lease_test.go`

**Interfaces:**
- Produces:
  ```go
  const (
  	Annotation  = "clusterplex.io/active-video-transcodes"
  	EntryPrefix = "clusterplex.io/video-transcodes."
  	RenewEvery  = 10 * time.Second
  	StaleAfter  = 30 * time.Second
  )
  type Counter struct {
  	Client              kubernetes.Interface
  	Namespace, Name     string
  	Identity            string // the pod name
  	Now                 func() time.Time
  	Logger              *slog.Logger
  }
  func (c *Counter) Begin(ctx context.Context) (end func()) // publishes at once; end publishes at once too
  func (c *Counter) Renew(ctx context.Context) error        // writes this pod's entry and the fresh sum
  func (c *Counter) Run(ctx context.Context)                // Renew every RenewEvery while the local count > 0
  func Count(l *coordinationv1.Lease, now time.Time) int    // the sum while renewTime is within StaleAfter, else 0
  ```

- [ ] **Step 1: Write the failing tests** (fake clientset, injected `Now`)

- **`TestACountIsPublishedWhileAConversionRuns`:** after `Begin`, the Lease exists with:
  - annotation `"1"`;
  - entry `video-transcodes.plex-0` = `"1 <unix>"`;
  - holder `plex-0`, `renewTime` = now, `leaseDurationSeconds` = 30.
- **`TestTheCountIsRenewedEveryTenSeconds`:** advance 10 s, call `Renew`, and `renewTime` has moved.
- **`TestTheLastEndPublishesZero`:** after `end()`, the annotation is `"0"` with no Renew needed.
- **`TestTwoManagersCountTogether`:** A and B each Begin, giving `"2"`; B ends, giving `"1"`.
- **`TestAStaleEntryLeavesTheSum`:** A Begins, the clock moves 31 s, B Begins, giving `"1"`.
- **`TestCountReadsZeroWhenStale`:** `Count` is 2 at renew-29 s and 0 at renew-31 s. A missing or non-numeric annotation is 0.

Run: `go test ./pkg/video/lease/ -v`
Expected: FAIL.

- [ ] **Step 2: Implement**

Use get-mutate-update under `retry.RetryOnConflict`, creating the Lease if it is absent, as `pkg/route/publisher.go:70` does.

- [ ] **Step 3: Run the tests**

Run: `go test ./pkg/video/lease/ -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add pkg/video/lease
git commit -m 'video: the Lease clustarr yields to -- each manager'\''s count, summed over fresh entries, renewed every 10 s, stale after 30' -- pkg/video/lease
```

---

### Task 8: The relay, and the manager's wiring

**Files:**
- Create: `pkg/video/relay/relay.go`, `pkg/video/relay/relay_test.go` (a real gRPC server over `bufconn`, as `pkg/remux/relay/grpc_test.go` does)
- Modify: `cmd/manager/config.go` (flags), `cmd/manager/execserver.go`, `cmd/manager/main.go` (start `Counter.Run`), `pkg/telemetry/telemetry.go`
- Test: `cmd/manager/config_test.go`, `cmd/manager/execserver_test.go`

**Interfaces:**
- Consumes: `video.Parse`, `Job.Proto`, `SegmentListLine` and `SegmentName` (Tasks 4–5); `plexsession` and `remoteexec.WorkerLister` (Task 6); `lease.Counter` (Task 7).
- Produces:
  ```go
  type Relay struct {
  	Workers   remoteexec.WorkerLister
  	Dial      func(ctx context.Context, addr string) (videopb.VideoClient, io.Closer, error)
  	PMS       plexsession.PMS
  	Count     interface{ Begin(context.Context) func() }
  	Roots     []string // where a whole-file output may land besides cwd (Task 13)
  	Refused   func(video.Reason)
  	Startup   func(time.Duration) // clusterplex_video_startup_overhead_seconds
  	Logger    *slog.Logger
  }
  func (r *Relay) Execute(ctx context.Context, req *pb.ExecRequest, sink remoteexec.Sink) error // satisfies remoteexec.VideoRoute
  func GRPCDialer(ctx context.Context, addr string) (videopb.VideoClient, io.Closer, error)    // MaxCallRecvMsgSize 256 MiB, InitialWindowSize 1 MiB
  ```
- Manager flags (and `Config` fields of the same names):
  - `--video-selector`, default `app=plex-video-transcode`;
  - `--video-port`, default 50053;
  - `--video-lease-name`, default `cluster-plex-video-transcodes`;
  - `--video-output-roots`, comma-separated, default empty.

  An empty selector leaves `Dispatcher.Video` nil, so conversions are refused `no-pool`.

- [ ] **Step 1: Write the failing tests**

- **`TestSegmentsLandBeforeTheirLine`:**
  - A fake worker sends 3 `Segment`s and `Done`.
  - Each segment-list request that the fake PMS receives finds its named file already in `cwd`, whole.
  - The lines arrive in order and equal `SegmentListLine` of each.
  - The exit code is 0.
  - Under finding L's `pipeline`, the check runs as each line is read from the one request body; under `per-line`, at each POST.
- **`TestAFileNameOutsidePlexsPatternFailsTheJob`:** `../x.ts` gives exit 1 and stderr `video: worker-error: refusing file name "../x.ts"`, and nothing is written outside `cwd`.
- **`TestOnlyPlexsProgressReachesPlex`:** a key outside finding V is dropped. Finding V's keys are PUT with `X-Plex-Token` set to the job's token.
- **`TestAFullPoolIsRefusedFull`:** two workers that both answer `RESOURCE_EXHAUSTED` give `video: full`, and `Refused` receives `full`.
- **`TestAFullWorkerIsPassedOver`:** A answers `RESOURCE_EXHAUSTED` and B serves the job.
- **`TestNoReadyWorkerIsNoPool`.**
- **`TestAWorkerThatDiesMidStreamExitsNonZero`:**
  - the stream errors after one segment;
  - the job exits 1 with `video: worker-error`;
  - `Count`'s `end` has run;
  - the written segment stays and no line names a missing file.
- **`TestAStreamedInputIsReadFromPlexWithTheToken`:**
  - the job is a `livetv-hls` one;
  - the fake PMS, at `Addr`, serves 1 MiB at the input's path and requires the token;
  - the fake worker receives exactly those bytes as `Chunk`s, then `eof`.
- **`TestACancelledJobIsCancelled`:** cancelling ctx gives stderr `video: cancelled`; the worker's stream sees cancellation and `Refused` receives `cancelled`.
- **`TestTheStartupOverheadIsObserved`:** `Startup` receives (first segment written − job start) − (its End − Start).
- **`TestTheWorkerNeverSeesTheToken`:** the `Job` the fake worker receives marshals without the token or any URL.
- **`TestVideoFlagsDefault`**, in `cmd/manager`, checks the four defaults.
- **`TestTheVideoRouteIsWiredWhenASelectorIsSet`:** `execServices()` gives a `*relay.Relay` with the selector set and nil with it empty.

Run: `go test ./pkg/video/relay/ ./cmd/manager/ -v`
Expected: FAIL.

- [ ] **Step 2: Implement the relay**

The flow:
1. `video.Parse`; a failure refuses `shape`.
2. `Workers.ListReady`; an empty list refuses `no-pool`.
3. Order the workers: `hashring.New(names...).Locate(job.RingKey())` first, then the rest by name.
4. For each worker, `Dial` and `Convert`, then send the `Job`. On `RESOURCE_EXHAUSTED`, try the next. If every worker answers that way, refuse `full`; if any other dial or open failure is the last one, refuse `worker-error`.
5. `Count.Begin`, with `end` deferred.
6. Start a goroutine per streamed input. Each opens the input's `Source`: `PMS.Open(Source, token)` for an http URL, `os.Open(Source)` for a local path such as a subtitle in Plex's config volume, which the manager sees as Plex's process does. It sends chunks of at most 256 KiB, then `eof` (or `error` on a read failure).
7. Read the `Down` events:
   - `Segment`: `WriteFile(cwd, …, hlsName)`, then that segment's line, per finding L;
   - `Progress`: filter by V, then PUT;
   - `Done`: `Finish(sink, "video", …)`.

Rule as `video: <reason>: <detail>` throughout. `File`, `Manifest` and `Write` come in Task 13: until then they fail the job with `worker-error: unexpected <event>`. Never log the token.

- [ ] **Step 3: Wire the manager**

In `execServices()`:
```go
if cfg.VideoSelector != "" {
	d.Video = &relay.Relay{
		Workers: &remoteexec.PodWorkerLister{Client: …, Namespace: …, Self: …, Selector: cfg.VideoSelector, Port: cfg.VideoPort},
		Dial:    relay.GRPCDialer,
		PMS:     plexsession.PMS{Addr: "http://" + m.plexAddr},
		Count:   m.videoCount,
		…
	}
}
```
`m.videoCount` is a `lease.Counter{Name: cfg.VideoLeaseName, Identity: <pod name>}`, started with `go m.videoCount.Run(ctx)`.

- [ ] **Step 4: Run the tests**

Run: `go test ./pkg/video/relay/ ./cmd/manager/ -v && make test`
Expected: PASS. `cmd/manager/noffgo_test.go` still passes, so the manager links no ffgo.

- [ ] **Step 5: Commit**

```bash
git add pkg/video/relay
git commit -m 'video: the relay -- a conversion goes to a worker with a free slot or is refused by reason; each segment lands before its line, inputs Plex serves are streamed with the session'\''s token, and the Lease counts it' -- pkg/video/relay cmd/manager pkg/telemetry
```

---

### Task 9: The device measure, and slots

**Files:**
- Create: `pkg/video/gpu/measure.go`, `pkg/video/gpu/measure_test.go`, `pkg/video/worker/slots.go`, `pkg/video/worker/slots_test.go`

**Interfaces:**
- Produces:
  ```go
  // pkg/video/gpu (links ffgo)
  type Caps struct {
  	Encode, Scale, Deinterlace, ToneMap bool
  	Decode                              map[string]bool // "h264:8", "hevc:10", …
  	Took                                time.Duration
  }
  func (c Caps) DecodesOnGPU(codec string, bits int) bool
  func Measure(ctx context.Context, renderNode string) (Caps, error) // an error when Encode or Scale fails
  // Clip encodes a testsrc2 clip on the CPU into MPEG-TS bytes in memory. Measure and the
  // GPU tests use it, so neither needs the ffmpeg CLI (absent from the image).
  func Clip(ctx context.Context, codec string, bits int, d time.Duration, w, h int) ([]byte, error)
  // pkg/video/worker
  func NewSlots(max int) *Slots
  func (s *Slots) Acquire(session string, kind video.OutputKind, cancel context.CancelFunc) (release func(), ok bool)
  func (s *Slots) Free() int
  ```

- [ ] **Step 1: Write the failing tests**

**`TestSlotsHoldOneBackForStreams`**, with `NewSlots(3)`:
1. stream A succeeds (2 free);
2. file F succeeds (1 free);
3. file G is refused;
4. stream B succeeds (0 free);
5. stream C is refused.

**`TestARestartTakesItsSessionsSlot`:** with A, B and C holding all three slots, `Acquire("A", hls, cancelA2)` succeeds, A's first cancel func has been called, and `Free()` is still 0.

**`TestMeasureWithoutADeviceFails`:** `Measure(ctx, "/dev/dri/none")` returns an error and a zero `Caps`.

**`TestTheNUCsDeviceMeasures`** runs only when `CLUSTERPLEX_EXPECT_INTEL=1`. It fails, rather than skips, when `/dev/dri/renderD128` is absent. `Encode`, `Scale` and `DecodesOnGPU("h264", 8)` are all true.

Run: `go test ./pkg/video/gpu/ ./pkg/video/worker/ -run 'Slots|Restart|Measure' -v`
Expected: FAIL.

- [ ] **Step 2: Implement**

`Measure` follows clustarr's `pkg/transcode/selfcheck/trial.go`:
- Encode one-second `testsrc2` clips per format in memory on the CPU: libx264 at 8 bits, libx265 at 8 and 10 bits, mpeg2video, and libvpx-vp9 at 8 and 10 bits.
- Decode each through a VAAPI `StreamDecoder`.
- Run 30 frames through `hwupload=extra_hw_frames=64,vpp_qsv=w=1280:h=720:format=nv12` into `h264_qsv`.
- Run the deinterlace and tone-map trials that findings D and H name.

- [ ] **Step 3: Run the tests**

Run: `make test-native && go test ./pkg/video/worker/ -v`
Expected: PASS. The GPU test is not run here.

- [ ] **Step 4: Commit**

```bash
git add pkg/video/gpu pkg/video/worker
git commit -m 'video: the worker proves its device and counts its slots -- one slot held for streams, a restart takes its session'\''s' -- pkg/video/gpu pkg/video/worker
```

---

### Task 10: The pipeline for HLS

**Files:**
- Create: `pkg/video/pipeline/pipeline.go` (`Run`), `input.go`, `video.go`, `audio.go`, `cut.go`, `pipeline_test.go`

**Interfaces:**
- Consumes: `video.Job`, `KeyframeDue`, `CutDue` and `SegmentName` (Tasks 4–5); ffgo `.14` (Task 3); `gpu.Caps` (Task 9).
- Produces:
  ```go
  type Options struct {
  	Encoder    string                 // "h264_qsv", or "libx264" where no GPU is present
  	Caps       *gpu.Caps              // nil: decode and scale on the CPU
  	RenderNode string
  	Inputs     func(i int) io.Reader  // a streamed input's bytes
  	InputsDir  string                 // the memory-backed emptyDir (Task 12)
  }
  type Segment struct { video.SegmentTimes; Data []byte }
  type Sink interface {
  	Segment(s Segment) error
  	File(name string, data []byte) error   // DASH (Task 13)
  	Manifest(mpd []byte) error             // DASH (Task 13)
  	Write(off int64, p []byte) error       // whole file (Task 13)
  	Progress(path, query string)
  }
  func Run(ctx context.Context, job video.Job, o Options, sink Sink) error
  ```

- [ ] **Step 1: Write the failing tests**

All of these run in CPU mode (`Encoder: "libx264"`, `Caps: nil`), on clips made with the `ffmpeg` CLI as `pkg/remux/pipeline/pipeline_test.go`'s `clip` makes them, and they honour `CLUSTERPLEX_REQUIRE_FFGO`. The job is the `library-hls` fixture with its input replaced.

- **`TestSegmentsStartOnKeyframesEverySegmentTime`:**
  - the clip is 40 s of HEVC 1080p, 25 fps, with a 10 s GOP;
  - the segments are `media-00000.ts` to `media-00004.ts`;
  - each decodes and begins with an IDR;
  - each lasts 8 s ± one frame, except the last.
- **`TestARestartNumbersFromItsStartNumber`:** with `-ss 16` and start number 2, the first segment is `media-00002.ts`, and its first pts is within one frame of 16 s.
- **`TestTheOutputIsWhatPlexAskedFor`:**
  - video: 720x404 H.264, 8-bit 4:2:0;
  - constant frame rate 24000/1001 from a 25 fps source, so the frame count is the duration × 23.976 ± 1;
  - audio: AAC stereo at 96 kHz, 232 kb/s ± 10%.
- **`TestTheBitrateStaysUnderMaxrate`:** no window of twice the bufsize exceeds 1.1 × maxrate.
- **`TestTheSegmentTimesAreThoseItsLineReports`:** each `Segment`'s Start and End match its decoded first pts and its last pts plus duration.
- **`TestAStreamedInputConverts`:** an mpegts clip, given through `Inputs(0)` from an `io.Pipe`, produces segments.
- **`TestACancelledConversionStops`:** cancelling after the first segment makes `Run` return `context.Canceled` within 2 s.
- **`TestTheQSVPipelineMeetsTheStartupTarget`** runs only under `CLUSTERPLEX_EXPECT_INTEL=1`, with `h264_qsv`, `Measure`'s `Caps`, and input from `gpu.Clip` (60 s of H.264 1080p):
  - with the input fed at real time, the first segment arrives within the segment duration plus 2 s;
  - with a file input, the encode runs at 5× or faster;
  - no 2 × bufsize window exceeds 1.1 × maxrate.

Run: `make test-native`
Expected: FAIL, undefined.

- [ ] **Step 2: Implement**

**Input.**
- A path is opened with `NewDecoderWithOptions(path, {Interrupt: ctx.Done(), ProbeSizeBytes: 20 MB, AnalyzeDuration: 20 s})`.
- A streamed input is opened with `NewDecoderFromReaderWithOptions`, its format from `Input.Format`. Interrupt is ignored on this path, so the reader returns an error once ctx is done.
- `job.Start` is honoured with `SeekPrecise`, and timestamps stay in source time, as `-copyts` asks.

**Video.** The chain depends on where the frame was decoded:

| decoded | chain | device |
|---|---|---|
| on the GPU (`Caps.DecodesOnGPU`; `StreamDecoderConfig{HWDevice: VAAPI, ExtraHWFrames: 16}`) | `hwmap=derive_device=qsv,format=qsv,vpp_qsv=w=%d:h=%d:format=nv12:framerate=%d/%d` | — |
| on the CPU | `fps=%d/%d,hwupload=extra_hw_frames=64,vpp_qsv=w=%d:h=%d:format=nv12` | QSV |
| CPU mode | `fps=%d/%d,scale=w=%d:h=%d,format=yuv420p` | — |

Before encoding each frame, call `SetPictureType(I)` when `KeyframeDue(t, forced, job.Video.KeyframeEvery)`, with `t` the frame's output time.

**The encoder.**
- `h264_qsv` with `CodecOptions{"forced_idr": "1", "async_depth": "1", "look_ahead": "0"}`, its rate control capped by MaxRate and BufSize. The mode within that is the builder's call, held by the bitrate tests.
- libx264 in CPU mode, with `CRF`, `MaxBitrate`, `BufferSize` and `CodecOptions{"forced-idr": "1"}`.

**Audio.** Decode, then `ffgo.Resampler` to the job's Channels and SampleRate, then `NewAudioEncoder{EncoderName: "aac", BitRate}`. This is `pkg/remux/pipeline`'s `audio` type with the job's numbers. `Copy` copies the packets.

**The cutter.**
- One `NewMuxerToWriter(&buf, "mpegts")` per job, with `WriteHeaderWithOptions({"mpegts_copyts": "1", "max_delay": "5000000", "avoid_negative_ts": "disabled"})`.
- Packets of every stream are merged in dts order.
- Before writing a video keyframe for which `CutDue` is true: call `Drain()`, emit `buf` as the next segment (named with `SegmentName(pattern, StartNumber+k)`, with its times), and reset `buf`.
- At the end, `WriteTrailer` and emit the last segment.

**Progress.** Send finding V's keys once a second.

- [ ] **Step 3: Run the tests**

Run: `make test-native`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add pkg/video/pipeline
git commit -m 'video: the pool'\''s HLS pipeline -- VA-API or CPU decode, vpp_qsv scale, h264_qsv with Plex'\''s forced keyframes, AAC, and MPEG-TS cut in memory' -- pkg/video/pipeline
```

---

### Task 11: Deinterlacing and tone mapping

**Files:**
- Modify: `pkg/video/pipeline/video.go`
- Test: `pkg/video/pipeline/filters_test.go`

**Interfaces:**
- Consumes: `VideoOut.Deinterlace` and `ToneMap` (Task 4, from findings D and H); `Caps.Deinterlace` and `ToneMap` (Task 9).

- [ ] **Step 1: Write the failing tests** (CPU mode, plus the GPU case under `CLUSTERPLEX_EXPECT_INTEL=1`)

- **`TestAnInterlacedSourceComesOutProgressive`:** a 1080i MPEG-2 clip (`-flags +ildct+ilme -top 1`) gives progressive frames (`field_order` progressive) at the job's rate.
- **`TestAnHDR10SourceComesOutSDR`:** an HEVC Main10 clip tagged bt2020 and smpte2084 gives 8-bit output with primaries, transfer and matrix all bt709.

Run: `make test-native`
Expected: FAIL.

- [ ] **Step 2: Implement**

**On the GPU,** for jobs the device measured:
- deinterlacing is `vpp_qsv`'s `deinterlace=2`;
- tone mapping is `vpp_qsv`'s `tonemap=1`, or `tonemap_vaapi` on VA-API frames, whichever `Measure` found working.

**On the CPU,** in CPU mode and for CPU-decoded frames:
- deinterlacing is `bwdif`;
- tone mapping is `zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=hable,zscale=t=bt709:m=bt709:r=tv,format=yuv420p`.

A job that asks for either on a device that measured neither runs that filter on the CPU before `hwupload`.

- [ ] **Step 3: Run the tests**

Run: `make test-native`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git commit -m 'video: interlaced channels and HDR films convert -- on vpp_qsv where the device measured it, else on the CPU before upload' -- pkg/video/pipeline
```

---

### Task 12: Burn-in

**Files:**
- Create: `pkg/video/pipeline/burn.go`, `composite.go`, `burn_test.go`, and `sup_test.go` (the PGS writer from Task 3's test, copied)

**Interfaces:**
- Consumes: `video.BurnIn` (finding B); `ffgo.Decoder.NewSubtitleStreamDecoder` and `SubtitleBitmap` (Task 3); `Options.InputsDir`.
- Produces: `func composite(f ffgo.Frame, bitmaps []ffgo.SubtitleBitmap, canvasW, canvasH int) error`, which blends onto yuv420p and scales each rect by the frame's size over the canvas size, bilinear. Also `func filterEscape(path string) string`.

- [ ] **Step 1: Write the failing tests** (CPU mode)

- **`TestATextSubtitleIsBurnedIn`:** an SRT sidecar reading "HELLO" from 1 s to 3 s. At 2 s, the bottom 20% of the frame's luma differs from the same job without burn-in by more than 5%. At 5 s it differs by less than 1%.
- **`TestAnEmbeddedTextTrackIsBurnedIn`:** the same check with an ASS track in an MKV.
- **`TestAnImageSubtitleIsComposited`:** an MKV with a PGS track (red, 64×32 at (100,600) on a 1920×1080 canvas). On a 720×404 output, the scaled rect's pixels are red-dominant (V > U+40) during its span and untouched outside it.
- **`TestAPathWithAQuoteAndAColonBurnsIn`:** a sidecar under `t.TempDir()/Schindler's List: Part 1, (1993)/sub.srt` burns in.
- **`TestAStreamedSubtitleIsHeldInMemoryOnly`:** an `Inputs[1].Streamed` subtitle is written under `InputsDir/<session>/`, and that directory is gone once `Run` returns.

Run: `make test-native`
Expected: FAIL.

- [ ] **Step 2: Implement**

- A burn-in job decodes and scales on the CPU (`scale`). A text subtitle goes through `subtitles=filename=<filterEscape(path)>:si=<n>` in the same chain. An image subtitle is passed to `composite` from the subtitle stream decoder, keeping the events active at each frame's pts.
- The frame is then uploaded: `hwupload=extra_hw_frames=64,format=qsv` into `h264_qsv`, or straight into libx264 in CPU mode.
- `filterEscape` escapes `\`, `'`, `:`, `,`, `;`, `[` and `]` for an FFmpeg filter argument.

- [ ] **Step 3: Run the tests**

Run: `make test-native`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add pkg/video/pipeline/burn.go pkg/video/pipeline/composite.go pkg/video/pipeline/burn_test.go pkg/video/pipeline/sup_test.go
git commit -m 'video: subtitles burned in -- text through libass, PGS composited from its bitmaps, a subtitle Plex downloaded held in memory only' -- pkg/video/pipeline
```

---

### Task 13: DASH with an encode, and whole-file jobs

**Files:**
- Create: `pkg/video/pipeline/dash.go`, `pkg/video/pipeline/file.go`, `pkg/video/pipeline/outputs_test.go`
- Modify: `pkg/video/relay/relay.go` (handle `File`, `Manifest` and `Write`; enforce `Roots`), `pkg/video/relay/relay_test.go`
- Create, only if finding F names `+faststart`: `pkg/video/faststart/faststart.go` and `faststart_test.go`

**Interfaces:**
- Consumes: `remux.Splitter`, `remux.Manifest`, `remux.InitFile` and `remux.ChunkFile` (`pkg/remux`); the remux pipeline's `fragmented` muxer options (`pkg/remux/pipeline/pipeline.go:44-48`, copied, since they are unexported); `ffgo.NewMuxerToWriteSeeker` (Task 3).
- Produces, if F names faststart: `func Relocate(f *os.File) error` moves `moov` before `mdat` and adds its size to every `stco`/`co64` entry, as qt-faststart does.

- [ ] **Step 1: Write the failing tests**

- **`TestADashEncodeKeepsTheRemuxPoolsContract`**, on the `dash-encode` fixture over an HEVC clip:
  - files `init-stream0.m4s`, `init-stream1.m4s` and `chunk-stream{0,1}-0000N.m4s`;
  - each video chunk starts on a keyframe;
  - one `Manifest` is rendered by `remux.Manifest`.
- **`TestAWholeFileIsWrittenThroughOffsets`**, on the `download` fixture's container:
  - applying every `Write` to a buffer gives a file that decodes fully, its duration within a frame;
  - no `Write` exceeds 1 MiB;
  - at least one `Write` lands below an earlier offset, which proves seeks pass through.
- **`TestAWholeFileLandsOnlyWhenDone`** (relay): the writes go to `<dir>/.<name>.part`, which is renamed at a successful `Done`; after a worker error, the `.part` file is gone and no output exists.
- **`TestAWholeFileOutsideTheRootsIsRefused`** (relay): an output path outside `cwd` and `Roots` is refused `shape` before any worker is dialled.
- **`TestFaststartMovesTheMovieBoxFirst`**, only with F's faststart: after `Relocate`, the top-level boxes are ftyp, moov, mdat, and the file decodes.

Run: `make test-native && go test ./pkg/video/relay/ -v`
Expected: FAIL.

- [ ] **Step 2: Implement**

- **DASH:** one `NewMuxerToWriter(&remux.Splitter{…}, "mp4")` per representation, with the fragmented options. `Flush` at each forced keyframe, which the 1-segment GOP puts on every boundary. Emit `File` per init and fragment, and a `Manifest` after each fragment, as the remux pool does.
- **File:** `NewMuxerToWriteSeeker(w, job.File.Format)` with `job.File.MuxerOptions`. `w` coalesces into writes of at most 1 MiB, and a seek flushes first. Whole-file jobs are not paced.
- **The relay:** `File` goes through `WriteFile` with the DASH name pattern, and `Manifest` is POSTed to `ManifestURL`. `Write` is `pwrite` into the `.part` file; at a successful `Done` it is `Relocate`d if F says so, then renamed.

- [ ] **Step 3: Run the tests**

Run: `make test-native && go test ./pkg/video/... -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add pkg/video/pipeline/dash.go pkg/video/pipeline/file.go pkg/video/pipeline/outputs_test.go
git commit -m 'video: Plex Web'\''s converted DASH and whole-file jobs -- downloads, Optimize and DVR conversions streamed as offset writes, landed only when whole' -- pkg/video
```

---

### Task 14: The worker server and `cmd/video-worker`

**Files:**
- Create: `pkg/video/worker/server.go`, `pkg/video/worker/server_test.go`, `cmd/video-worker/main.go`, `cmd/video-worker/main_test.go`
- Modify: `Makefile` (`build` adds `bin/video-worker`), `pkg/telemetry/telemetry.go`

**Interfaces:**
- Consumes: `Slots` (Task 9), `pipeline.Run` (Task 10), `gpu.Measure`, `videopb`.
- Produces:
  ```go
  type Server struct {
  	videopb.UnimplementedVideoServer
  	Slots        *Slots
  	Run          func(ctx context.Context, job video.Job, o pipeline.Options, sink pipeline.Sink) error
  	Options      pipeline.Options
  	InputStall   time.Duration // default 30 s
  	Logger       *slog.Logger
  }
  func (s *Server) Convert(stream videopb.Video_ConvertServer) error
  func (s *Server) Ready() bool // device measured and Slots.Free() > 0
  ```
- Flags:
  - `--listen :50053`, `--probe :8080`, `--max-jobs 3`;
  - `--render-node /dev/dri/renderD128`, `--encoder h264_qsv`;
  - `--inputs-dir /run/video-inputs`;
  - `--self-check`: print the runtime and measure, then exit.
- Metrics: `clusterplex_video_worker_active_jobs{kind}` and `clusterplex_video_worker_encode_speed` (a histogram of media seconds per wall second, per job).

- [ ] **Step 1: Write the failing tests** (a fake `Run`, over `bufconn`)

- **`TestAJobBeyondTheSlotsIsResourceExhausted`:** with `max-jobs` 1 and a job holding the slot, a second job's first `Recv` returns `codes.ResourceExhausted`.
- **`TestStreamedInputChunksReachThePipeline`:** the fake `Run` reads `Options.Inputs(0)` and gets the chunks' bytes in order, then EOF.
- **`TestEveryConversionEndsWithDone`:** both success and a `Run` error end with exactly one `Done`, the error carrying `reason: worker-error`.
- **`TestAStalledStreamedInputReleasesItsSlot`:** with `InputStall` at 100 ms and no chunk sent, the job ends with `Done{reason: worker-error, error: "input stalled"}` and `Slots.Free()` returns to max.
- **`TestReadyzFollowsTheDeviceAndTheSlots`:** unmeasured gives 503; measured with a free slot gives 200; full gives 503.
- **`TestTheVideoWorkerRefusesAnOldRuntime`:** `checkRuntime`, as in Task 2, with the shim minimum Task 3 set.

Run: `go test ./pkg/video/worker/ ./cmd/video-worker/ -v`
Expected: FAIL.

- [ ] **Step 2: Implement**

- The server options are `grpc.InitialWindowSize(1<<20)`, `grpc.InitialConnWindowSize(1<<22)`, `grpc.MaxRecvMsgSize(1<<20)` and `grpc.MaxSendMsgSize(256<<20)`. The bounded windows make a relay that stops reading stall the pipeline's `Send` (Task 15).
- Each streamed input is an `io.Pipe` that a goroutine fills from the `Up` chunks. A watchdog fails the job after `InputStall` without a chunk.
- At start, `main` runs `ffgo.Init`, then `checkRuntime`, then `gpu.Measure` unless `--encoder libx264`. `/readyz` answers `Server.Ready()`.

- [ ] **Step 3: Run the tests**

Run: `go test ./pkg/video/... ./cmd/video-worker/ -v && make build && make test`
Expected: PASS, and `bin/video-worker` is built.

- [ ] **Step 4: Commit**

```bash
git add pkg/video/worker/server.go pkg/video/worker/server_test.go cmd/video-worker
git commit -m 'video-worker: the GPU pool'\''s gRPC server -- slots, streamed inputs with a stall timeout, readiness from the device and the slots' -- pkg/video/worker cmd/video-worker Makefile pkg/telemetry
```

---

### Task 15: Pacing as PMS paces its transcoder

**Files:**
- Modify: `pkg/video/relay/relay.go`, `pkg/video/relay/relay_test.go`
- If P is `stopped`, also: `proto/transcoder.proto` (`ExecRequest` gains `int32 pid = 6`), `cmd/shim/main.go` (sends `os.Getpid()`), and a new `pkg/video/relay/pace.go`

**Interfaces:**
- Consumes: finding P; the worker's bounded flow-control windows (Task 14).
- Produces, if P is `stopped`: `type StateReader func(pid int) (byte, error)`, defaulting to field 3 of `/proc/<pid>/stat`, as `Relay.State`. The manager shares the shim's pid namespace, since it starts Plex under its subreaper.

Every alternative holds a stream the same way: the relay stops receiving `Down` events, and the worker's pipeline then blocks in `Send`. The worker's windows bound what is in flight.

- [ ] **Step 1: Write the failing tests** (keep the one for finding P, plus the common one)

- **Common, `TestAHeldRelayStallsThePipeline`:** a real worker server runs a fake pipeline that emits a 2 MiB segment every 10 ms. While the relay is held, the pipeline completes at most 2 more `Segment` calls. Once released, it continues.
- **P = `stopped`, `TestAStoppedShimHoldsTheWorker`:** the fake `State` returns `T`. Within 250 ms the relay stops receiving, and once `State` returns `S` it resumes.
- **P = `unread`, `TestAnUnreadSegmentListHoldsTheWorker`:** the fake PMS stops reading the segment-list body, and the relay stops receiving within one segment.
- **P = `answer`, `TestPlexsThrottleAnswerHoldsTheWorker`:** the fake PMS answers as finding P records, and the relay holds until PMS answers otherwise.

Run: `go test ./pkg/video/relay/ -run 'Held|Stopped|Unread|Throttle' -v`
Expected: FAIL.

- [ ] **Step 2: Implement the alternative P names, then run the tests**

Run: `go test ./pkg/video/... ./pkg/remoteexec/... -v && make test`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git commit -m 'video: a conversion runs at the pace PMS sets its transcoder -- the relay holds the worker as PMS holds Plex'\''s own (finding P)' -- pkg/video/relay proto cmd/shim
```

---

### Task 16: The image

**Files:**
- Create: `images/Dockerfile.video`
- Modify: `images/stage.sh` (the binary path comes from `BINARY`, defaulting to `/usr/bin/remux-worker`), `Makefile`, `.github/workflows/images.yml`
- Test: `hack/video-selfcheck.sh`

**Interfaces:**
- Produces: the image `ghcr.io/mediactl/cluster-plex-video`, amd64 only, with `/usr/bin/video-worker`.

- [ ] **Step 1: Write the self-check script**

`hack/video-selfcheck.sh <image>` runs `docker run --rm <image> /usr/bin/video-worker --self-check --encoder libx264` and fails unless the output has all of these lines:
- `libavcodec 63`;
- `shim api` at Task 3's minimum or above;
- `encoder h264_qsv: found` and `encoder libx264: found`;
- `filter <f>: found` for each of `vpp_qsv`, `hwmap`, `hwupload`, `subtitles`, `bwdif`, `zscale`, `tonemap`;
- `driver /usr/lib/x86_64-linux-gnu/dri/iHD_drv_video.so: present`.

`--self-check` with libx264 skips `Measure`, so it runs without a GPU.

Run it against an image that does not exist yet.
Expected: FAIL.

- [ ] **Step 2: Write the Dockerfile**

`images/Dockerfile.video` has these stages:
- **build:** `images/Dockerfile.remux`'s, building `./cmd/video-worker` with `CGO_ENABLED=0`, and copying ffgo's `shim/` and LICENSE out of the module directory.
- **ffmpeg:** the same BtbN pin and amd64 SHA-256 as `Dockerfile.remux`, building `libffshim.so`.
- **intel:** clustarr's `images/Dockerfile.native` Intel stage, its apt sources and packages copied: `intel-media-va-driver-non-free`, `libmfx-gen1.2`, `libva2` (2.21 or later), `libva-drm2`.
- **staging:** `BINARY=/usr/bin/video-worker`, with `EXTRA_DLOPEN` naming `iHD_drv_video.so`, `libmfx-gen.so.1.2` and the `libvpl` libavcodec loads (check with `ldd` in the stage).
- **video:** FROM scratch, `USER 1000:1000`, `ENV LIBVA_DRIVER_NAME=iHD LIBVA_DRIVERS_PATH=/usr/lib/x86_64-linux-gnu/dri FFGO_SHIM_DIR=/usr/lib`.
- **video-test:** `video` plus `go test -c` binaries of `pkg/video/gpu` and `pkg/video/pipeline`, at `/usr/bin/gpu.test` and `/usr/bin/pipeline.test`. Only Task 19 runs it, on the NUC.

Makefile: `VIDEO_IMG ?= ghcr.io/mediactl/cluster-plex-video:dev`, with targets `docker-build-video` (`--target video`), `docker-buildx-video` (`--platform linux/amd64`) and `docker-selfcheck-video`.

In CI, add `cluster-plex-video` to both matrices, amd64 only. The merge job asserts the digest count per image.

- [ ] **Step 3: Build and check**

Run: `make docker-build-video && hack/video-selfcheck.sh ghcr.io/mediactl/cluster-plex-video:dev`
Expected: the build succeeds, every line is present, and the exit code is 0. `stage.sh` still refuses `ffmpeg` and `ffprobe`.

- [ ] **Step 4: Commit**

```bash
git add images/Dockerfile.video hack/video-selfcheck.sh
git commit -m 'images: cluster-plex-video -- FFmpeg 9 with Intel'\''s media driver and oneVPL for the GPU pool, amd64, no ffmpeg executable' -- images Makefile .github/workflows/images.yml hack/video-selfcheck.sh
```

---

### Task 17: The chart and the docs

**Files:**
- Create: `charts/cluster-plex/templates/video-transcode.yaml`, `k8s/base/video-transcode.yaml`, `test/manifests/video_test.go`, `docs/adr/0010-video-conversions-on-the-gpu-pool.md`
- Modify: `charts/cluster-plex/values.yaml`, `charts/cluster-plex/templates/storage.yaml` (the manager's config keys), `charts/cluster-plex/Chart.yaml` (version `0.5.0`), `test/manifests/placement_test.go`, `docs/configuration.md`, `CLAUDE.md`, `docs/adr/0007-browser-streams-on-the-remux-pool.md` (a pointer to 0010)

**Interfaces:**
- Produces these values:
  ```yaml
  videoTranscode:
    enabled: false
    image: {repository: ghcr.io/mediactl/cluster-plex-video, tag: ""}
    replicas: 1
    maxJobs: 3
    encoder: h264_qsv
    renderNode: /dev/dri/renderD128
    renderGroups: []
    gpu: {resource: gpu.intel.com/i915, count: 1}
    nodeSelector: {}
    affinity:
      nodeAffinity:
        requiredDuringSchedulingIgnoredDuringExecution:
          nodeSelectorTerms:
            - matchExpressions:
                - {key: intel.feature.node.kubernetes.io/gpu, operator: In, values: ["true"]}
    tolerations: []
    resources: {requests: {cpu: "2", memory: 2Gi}, limits: {memory: 4Gi}}
    inputs: {sizeLimit: 64Mi}
    outputRoots: []
  ```
- The manager config keys:
  - `video-selector`: `app=plex-video-transcode` when enabled, `""` when not;
  - `video-port`: `50053`;
  - `video-lease-name`;
  - `video-output-roots`.

- [ ] **Step 1: Write the failing tests**

**`TestTheVideoPoolAsksForOneGPUShareOnAnIntelNode`** renders with `videoTranscode.enabled=true`. The Deployment `plex-video-transcode` has:
- the label `app: plex-video-transcode`;
- `resources.limits["gpu.intel.com/i915"] == "1"`;
- the required affinity on the Intel NFD label;
- `runAsNonRoot`, `readOnlyRootFilesystem` and dropped ALL capabilities;
- the media claim mounted read-only at `/media`;
- an `emptyDir{medium: Memory, sizeLimit: 64Mi}` at `/run/video-inputs`;
- args including `--max-jobs=3` and `--encoder=h264_qsv`;
- `supplementalGroups` equal to `renderGroups`.

**`TestADisabledVideoPoolRefusesConversions`:** without the pool there is no such Deployment, and the config has `video-selector: ""`.

**`TestTheManagerIsToldTheVideoPool`:** when enabled, the config has the selector, port 50053 and the lease name.

**`TestPlacementKeysReachEveryWorkload`** also covers `-video-transcode`.

Run: `go test ./test/manifests/ -v`
Expected: FAIL.

- [ ] **Step 2: Implement**

- The template follows `templates/remux.yaml`, plus the GPU limit, affinity, groups and the memory `emptyDir`. Probes are on `/readyz` and `/healthz`, and there is no HPA.
- The kustomize base mirrors it.
- **ADR-0010** records:
  - every video conversion on the GPU pool, with no fallback;
  - in-memory muxing (owner);
  - the Lease;
  - "Consequences": Plex for iOS Live TV plays; with the pool down, conversions are refused; clustarr's Intel admission pauses while anyone watches a converted stream.
- `docs/configuration.md` gains the `videoTranscode` values and the four manager flags. `CLAUDE.md` gets a line under its architecture section.

- [ ] **Step 3: Run the tests**

Run: `go test ./test/manifests/ -v && make helm-lint && make test`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add charts/cluster-plex/templates/video-transcode.yaml k8s/base/video-transcode.yaml test/manifests/video_test.go docs/adr/0010-video-conversions-on-the-gpu-pool.md
git commit -m 'chart: videoTranscode -- the GPU pool on the Intel node, one i915 share a pod; without it Plex'\''s video conversions are refused, never run on Plex'\''s node (chart 0.5.0, ADR-0010)' -- charts k8s test/manifests docs CLAUDE.md
```

---

### Task 18: clustarr's Intel admission yields

**Files** (all in `/home/appkins/src/mediactl/clustarr`):
- Create: `app/transcode/admission/videolease.go`, `app/transcode/admission/videolease_test.go`
- Modify: `app/transcode/admission/pass.go`, `controller.go` (the RBAC marker in the package-level block at :66–70, the Lease watch, the requeue), `app/transcode/manager/register.go`, `internal/cli/manager/options.go`, `command.go`, `run.go`
- Modify: `charts/clustarr/values.yaml` (`manager.videoTranscodeLease: ""`), `values.schema.json`, `templates/deployments.yaml` (`$managerArgs`, rendered only when set), `charts/clustarr/README.md`
- Test: `app/transcode/admission/ledger_test.go`, `internal/cli/manager/options_test.go`, `cmd/manager/manifest_test.go`

**Interfaces:**
- Produces:
  ```go
  const videoAnnotation = "clusterplex.io/active-video-transcodes"
  const videoStale = 30 * time.Second
  func videoStreams(l *coordinationv1.Lease, now time.Time) int // must agree with cluster-plex's lease.Count
  // PassInputs gains: VideoStreams int
  // Reconciler gains: VideoLease types.NamespacedName (zero: no yield)
  // Options gains: VideoTranscodeLease string // "<namespace>/<name>", --video-transcode-lease
  ```

- [ ] **Step 1: Write the failing tests**

- **`TestVideoStreamsReadsTheLease`:**
  - `"2"` renewed 29 s ago gives 2;
  - renewed 31 s ago gives 0;
  - a missing or garbled annotation gives 0;
  - a nil Lease gives 0.
- **`TestAFreshVideoCountHoldsNewIntelAdmissions`**, built on `newTestLedger` and `passIn`, with intel slots 2 and `VideoStreams: 1`:
  - an `auto` file is not admitted to intel;
  - a `hardware: gpu` file waits with reason `waiting: Plex is converting video on the GPU`.
- **`TestRunningIntelEncodesKeepRunningBesideAStream`:** a running intel slot is neither withdrawn nor rerouted while `VideoStreams > 0`.
- **`TestAdmissionResumesOnceTheCountIsZero`:** with `VideoStreams: 0`, the waiting file is admitted to intel.
- **`TestRejectsABadVideoTranscodeLeaseFlag`:** `nonamespace` and `a/b/c` fail `Validate`, and `""` passes. A namespace outside `--watch-namespace`, when that is set, fails too.
- **`TestManagerCarriesTheVideoTranscodeLeaseFlag`:** with `manager.videoTranscodeLease: clustarr/cluster-plex-video-transcodes`, the chart renders `--video-transcode-lease=clustarr/cluster-plex-video-transcodes`. Unset, it renders no such arg.

Run: `go test ./app/transcode/admission/ -run 'Video|Stream|Resumes' -v && go test ./internal/cli/manager/ ./cmd/manager/ -run 'VideoTranscodeLease' -v`
Expected: FAIL.

- [ ] **Step 2: Implement**

- `passInputs` reads the Lease with the cached client, as `app/livetv/controller/iptvprovider/blocks.go:300-320` does, and sets `VideoStreams`.
- When `VideoStreams > 0`, `Pass` clones the slots and sets `intel` to 0 before both `assignClasses` and `Admit`, so running and Queued files are untouched. Do not use `Unhealthy` or `Held`, which reroute queued files.
- The wait reason for intel-pinned files is the string above.
- `SetupWithManager` watches `coordinationv1.Lease`, filtered to `VideoLease`.
- While `VideoStreams > 0`, `Reconcile` requeues no later than the Lease's `renewTime` + 30 s + 1 s.
- The flag goes in beside `--slots` and flows through `transcodeOptions` into the `admission.Reconciler` literal.

- [ ] **Step 3: Run the gates**

Run:
```
go build ./... &&
go test ./app/transcode/admission/ ./internal/cli/manager/ ./cmd/manager/ ./app/transcode/manager/ &&
go test ./test/guards/ -run 'TestChartRBACMatchesTheGeneratedRoles|TestEveryPackageWithRBACMarkersIsInARole|TestEveryGeneratedRoleMatchesItsMarkers|TestRBACMarkersArePackageLevel|TestEveryBuiltinKindAControllerTouchesHasAnRBACMarker' &&
make manifests && git diff --exit-code config/ &&
make brand-gate && helm lint --strict charts/clustarr
```
Expected: all pass. `config/` is unchanged, since the manager role already grants leases get, list and watch.

- [ ] **Step 4: Commit**

```bash
git -C /home/appkins/src/mediactl/clustarr add app/transcode/admission/videolease.go app/transcode/admission/videolease_test.go
git -C /home/appkins/src/mediactl/clustarr commit -m 'feat(transcode): Intel admission yields to Plex'\''s video conversions -- while cluster-plex'\''s Lease counts a live conversion, no new file is admitted to intel; running encodes finish (--video-transcode-lease, chart manager.videoTranscodeLease)' -- app/transcode internal/cli/manager cmd/manager charts/clustarr
```

---

### Task 19: Publish, roll out to talos, and verify

**Files:**
- Modify: `/home/appkins/src/mediactl/clustarr/charts/clustarr/Chart.yaml` (the cluster-plex dependency to `0.5.0`; the chart version bumped)
- Modify: `~/src/tfc/cluster-bootstrap/clustarr/talos-values.yaml`, and `~/src/tfc/cluster-bootstrap/main.tf:331` (`chart_version`)
- Modify: the spec (a closing "Verified on talos" section), and the memory file `plex-video-transcode-gpu-pool.md`

- [ ] **Step 1: Ask the owner before publishing**

List what will be published:
- the cluster-plex image;
- the `cluster-plex-video` image;
- the clustarr image;
- cluster-plex chart 0.5.0;
- the clustarr chart version;
- a one-off test Pod on the NUC, which holds one i915 share for a few minutes (Step 2b).

Then wait for the owner's OK.

- [ ] **Step 2: Build and push**

- Build each image from a `git archive` export of its repo's HEAD, tagged `g<sha>` (an all-digit tag reads as a float in the Terraform path).
- Push the cluster-plex chart with `make chart-push`.
- Bump clustarr's dependency, run `helm dependency update charts/clustarr`, and publish its chart the way its release workflow does.

Expected: each push prints its digest.

- [ ] **Step 2b: Run the GPU tests on the NUC**

Run the `video-test` image as a Pod on `talos-88-ae-dd-75-3d-a0`, with:
- `gpu.intel.com/i915: 1`;
- `CLUSTERPLEX_EXPECT_INTEL=1`;
- the command `/usr/bin/gpu.test -test.run NUC -test.v`, then `/usr/bin/pipeline.test -test.run QSV -test.v`.

Read its logs, then delete the Pod.
Expected: both PASS, and none skip. A failure stops the rollout here.

- [ ] **Step 3: Edit the talos values**

Set these by YAML key, with a guarded block replace:
- `cluster-plex.videoTranscode.enabled: true`;
- `cluster-plex.videoTranscode.image.tag`;
- the cluster-plex and clustarr image tags;
- `manager.videoTranscodeLease: <release namespace>/cluster-plex-video-transcodes`. The namespace is the one `kubectl get pods -A -l app=plex` shows.

Set `chart_version` in `main.tf`. Commit through a temp `GIT_INDEX_FILE` (read-tree HEAD, add, write-tree, commit-tree, update-ref), then `git update-index --cacheinfo` for those paths.

- [ ] **Step 4: Plan**

- Announce the deploy to peer sessions.
- Run `TF_CLI_ARGS_plan=-no-color TF_CLI_ARGS_apply=-no-color hack/clustarr-deploy.sh`.
- Compare the rendered manifests with the live revision as parsed YAML.

Expected: the changes are:
- the new `plex-video-transcode` Deployment;
- the manager config's `video-*` keys;
- the clustarr manager's `--video-transcode-lease` arg;
- the image tags;
- whatever cluster-plex charts 0.4.2 and 0.4.3 changed, each listed and checked.

- [ ] **Step 5: Apply**

- Check that `clusterplex_plex_sessions` is 0.
- Run `hack/clustarr-deploy.sh --apply`, then `kubectl -n tinkerbell wait --for=jsonpath='{.status.revision}'=<N> helmreleaseproxy/clustarr-talos-qpg9x`.

Expected:
- `plex-video-transcode` is Ready on `talos-88-ae-dd-75-3d-a0`, and its start log lists the measured `Caps`;
- `video-selector` is non-empty in plex-0's manager config.

- [ ] **Step 6: Run the spec's seven checks, recording the numbers**

1. **Live TV on Plex for iOS.** `clusterplex_video_startup_overhead_seconds` is 2 s or less per job. Also record the tap-to-picture time.
2. **30 minutes of one channel.** No rebuffering, `clusterplex_video_worker_encode_speed` above 1×, and no `Plex Transcoder` process on plex-0's node throughout (sampled each minute with Task 1's process watch).
3. **Three conversions at once** (two channels and one film), with two clustarr intel encodes already running. If any stream falls below 1×, stop and bring the numbers to the owner.
4. **Burn-in, HDR and interlacing.** A film with burned-in subtitles, an HDR film converted to SDR, and an interlaced channel, if the lineup has one.
5. **An iOS download** that needs conversion completes and plays offline.
6. **clustarr's Intel admission** pauses while streams are live (`waiting: Plex is converting video on the GPU`) and resumes within a minute after the last ends.
7. **The pool scaled to 0** (`kubectl scale`, then restored). A tune is refused with `video: no-pool` or `video: full` in the manager log, and nothing runs on the Pi.

- [ ] **Step 7: Record the results**

- Write the "Verified on talos" section in the spec, with the numbers, and commit it with a pathspec.
- Update the memory file `plex-video-transcode-gpu-pool.md`: deployed, the revision, and the results.
