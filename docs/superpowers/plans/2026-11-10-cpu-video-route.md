# CPU Video Route Implementation Plan (cluster-plex half)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Plex's streamed conversions of H.264/MPEG-2 sources at 1080p or below without HDR run on a second, libx264 video pool pinned to the NUC whenever clustarr says it holds intel work (or always, by value), the GPU pool otherwise, each pool the other's second chance when full, with the GPU Lease counting GPU conversions alone.

**Architecture:** The video worker gains an eligibility check on the source it opens and answers `FailedPrecondition` for a source beyond its pool; the relay gains a second worker lister and orders the two pools per conversion, passing a pool over on `ResourceExhausted` (full) or `FailedPrecondition` (not for this pool); the manager reads clustarr's `clustarr-intel-work` Lease the way clustarr reads ours and wires the pools from three new flags; the chart renders the CPU Deployment and the flags. The clustarr half (the Lease writer) is `clustarr/docs/superpowers/plans/2026-11-10-intel-work-lease.md`; this half is complete on its own with `videoTranscode.cpu.when: always`.

**Tech Stack:** Go, ffgo (FFmpeg 9 in process), gRPC (`proto/videopb`), controller-runtime-free client-go, Helm.

**Spec:** `docs/superpowers/specs/2026-11-10-cpu-video-route-design.md` (approved 2026-11-10; its "Measured" section sizes the pool).

## Global Constraints

- Media work goes through ffgo only; the CPU path is the worker's existing software pipeline (`pkg/video/pipeline`), never an `ffmpeg` executable.
- The worker holds no Plex secret; a Live TV input is streamed by the manager as today.
- The pool pod writes nothing to disk; outputs are muxed in memory.
- Plex's own node runs no conversion: a job neither pool takes is refused as today (`no-pool`, `full`, `shape`, `worker-error`, `cancelled`), never run locally.
- Defaults from the spec: CPU pool `maxJobs 2`, requests `cpu: "3"`, `memory: 2Gi`, limits `cpu: "8"`, `memory: 4Gi`; eligibility `sources: [h264, mpeg2video]`, `maxSourcePixels: 2073600`, `hdr: false`; `when: clustarr-busy` (values `clustarr-busy`, `always`, `never`); libx264 `threads` = the CPU limit; the GPU pool unchanged.
- clustarr's Lease: `clustarr-intel-work` in clustarr's namespace, annotation `clustarr.io/intel-work: "<n>"`, renewed every 10 s, stale after 30 s (`lease.StaleAfter`).
- Commits go on `main` by pathspec (`git commit -m ... -- <paths>`), never a bare commit or `git stash`; new files are `git add`ed first. Tests: `go test ./pkg/video/relay/... ./pkg/video/worker/... ./pkg/video/lease/... ./cmd/...` for the plain suites and `make test-native` for `pkg/video/pipeline`; `make helm-lint` for the chart; `make lint` before the last commit.
- Bump `charts/cluster-plex/Chart.yaml`'s version with the chart change (the Makefile's rule: every chart change, never re-pushed).

## Review Focus

- A CPU pod that dies after accepting and before any output ends the stream with `Aborted` or `Unavailable`, not `FailedPrecondition`: that is a `worker-error` refusal, never a pass-over loop (Task 4's test `TestAWorkerThatDiesAfterAcceptingIsNotPassedOver`).
- A seek restarts the job (new `-ss`); it is routed afresh and may land on the other pool, so the placement is replaced, never duplicated (Task 4, `TestARestartIsPlacedAfresh`).
- clustarr's Lease unreadable (RBAC, absent, stale): the manager treats it as not busy and routes GPU first, logging once per job, never refusing (Task 3's `TestAnUnreadableWorkLeaseIsNotBusy`, Task 5's wiring).
- A job both pools pass over as ineligible (an HDR job on a `never` route with the GPU pool full) is refused `full` when any pod was full, else `no-pool`, with the reason counted (Task 4, `TestAJobNoPoolTakesIsRefusedOnce`).
- A Live TV input passed over by the CPU pool is streamed again to the GPU pool from the playlist's first segment, never resumed mid-stream (Task 4, `TestAStreamedInputIsPumpedAgainForTheSecondPool`).

---

### Task 1: Eligibility in the pipeline

**Files:**
- Modify: `pkg/video/job.go` (beside `ErrShape`)
- Create: `pkg/video/pipeline/eligible.go`
- Modify: `pkg/video/pipeline/pipeline.go` (`Options`, `Run` after `openInput`)
- Modify: `pkg/video/pipeline/video.go` (`openEncoder`, the libx264 branch)
- Test: `pkg/video/pipeline/eligible_test.go`, `pkg/video/pipeline/filters_test.go`

**Interfaces:**
- Produces: `video.ErrNotForPool error` (text `not for this pool`); `pipeline.Source{Codec string; Width, Height, Bits int; HDR bool}`; `pipeline.Eligibility{Sources []string; MaxPixels int; HDR bool}` with `func (e Eligibility) Check(s Source) error` (nil, or `fmt.Errorf("%w: ...", video.ErrNotForPool)` naming the field: `"not for this pool: hevc"`, `"not for this pool: 3840x2160"`, `"not for this pool: HDR"`); `Options.Eligible *Eligibility` (nil: everything); `Options.Threads int` (0: libx264's default).
- `sourceOf(in *input) Source` (unexported): `Codec` from `in.video.Codec`, `Bits` 10 for a `10`/`p010` pixel format else 8, `HDR` when the transfer is PQ or HLG or the 10-bit BT.2020 "may be HDR" case `newVideoPath` already computes (lift that expression into a shared `mayBeHDR(vs *ffgo.StreamInfo, bits int) bool`).

- [ ] **Step 1: Write the failing unit test** in `pkg/video/pipeline/eligible_test.go`:

```go
func TestEligibilityJudgesCodecSizeAndHDR(t *testing.T) {
	e := pipeline.Eligibility{Sources: []string{"h264", "mpeg2video"}, MaxPixels: 1920 * 1080, HDR: false}
	ok := pipeline.Source{Codec: "h264", Width: 1920, Height: 1080, Bits: 8}
	assert.NoError(t, e.Check(ok))
	assert.NoError(t, e.Check(pipeline.Source{Codec: "mpeg2video", Width: 1920, Height: 1088, Bits: 8}), "1088 coded lines are 1080p") // MaxPixels compares Width*Height against 1920*1088 rounded: treat 1920x1088 as eligible by allowing 16 lines of coding slack
	for _, bad := range []pipeline.Source{
		{Codec: "hevc", Width: 1920, Height: 1080, Bits: 8},
		{Codec: "h264", Width: 3840, Height: 2160, Bits: 8},
		{Codec: "h264", Width: 1920, Height: 1080, Bits: 10, HDR: true},
	} {
		err := e.Check(bad)
		require.ErrorIs(t, err, video.ErrNotForPool, bad)
	}
	assert.NoError(t, pipeline.Eligibility{}.Check(pipeline.Source{Codec: "hevc", Width: 7680, Height: 4320, HDR: true}), "an empty rule takes everything")
	assert.Error(t, pipeline.Eligibility{HDR: false, Sources: []string{"h264"}}.Check(pipeline.Source{Codec: "h264", Width: 1280, Height: 720, HDR: true}))
}
```

The coding-slack rule: `Check` refuses when `Width*Height > MaxPixels` after rounding `Height` down to a multiple of 16 when it is within 16 of such a multiple; write that as `codedHeight(h int) int` in the same file.

- [ ] **Step 2: Run it to make sure it fails**

Run: `FFGO_SHIM_DIR=$PWD/bin CLUSTERPLEX_REQUIRE_FFGO=1 go test ./pkg/video/pipeline/ -run TestEligibilityJudgesCodecSizeAndHDR`
Expected: FAIL, `undefined: pipeline.Eligibility`

- [ ] **Step 3: Implement** `video.ErrNotForPool` in `pkg/video/job.go`, `Source`, `Eligibility`, `Check`, `codedHeight` in `pkg/video/pipeline/eligible.go`, and `Options.Eligible`, `Options.Threads` in `pkg/video/pipeline/pipeline.go`. `Sources` empty means any codec; `MaxPixels` 0 any size; `HDR` true allows HDR.

- [ ] **Step 4: Call it in `Run`**: in `pkg/video/pipeline/pipeline.go`, right after `in, err := openInput(...)` and its `defer in.close()`, add `if o.Eligible != nil { if err := o.Eligible.Check(sourceOf(in)); err != nil { return err } }` so it runs before `openDevices`, `newBurner` and any sink output. Add `sourceOf` and `mayBeHDR` (moved out of `newVideoPath`, which now calls it).

- [ ] **Step 5: Threads**: in `openEncoder`'s CPU branch (`pkg/video/pipeline/video.go`, the `else` after `if v.o.onGPU()`), add `if v.o.Threads > 0 { cfg.CodecOptions["threads"] = strconv.Itoa(v.o.Threads) }`.

- [ ] **Step 6: Write the native failing test** in `pkg/video/pipeline/filters_test.go`, beside `TestAnHDR10SourceComesOutSDR` (reuse its job and sink helpers):

```go
func TestAnIneligibleSourceIsNotForThePoolBeforeAnyOutput(t *testing.T) {
	// hdr10.mkv through a CPU pod that takes no HDR: refused before a segment.
	job, sink := hdr10Job(t) // the helper TestAnHDR10SourceComesOutSDR builds its job and sink with
	o := cpuOptions(t)       // likewise
	o.Eligible = &pipeline.Eligibility{HDR: false}
	err := pipeline.Run(t.Context(), job, o, sink)
	require.ErrorIs(t, err, video.ErrNotForPool)
	assert.Empty(t, sink.segments, "nothing was produced")
	o.Eligible = &pipeline.Eligibility{HDR: true}
	require.NoError(t, pipeline.Run(t.Context(), job, o, sink), "the same rule allowing HDR runs it")
}
```

If the existing test inlines its job and sink rather than through helpers, extract `hdr10Job(t)` and `cpuOptions(t)` from it first.

- [ ] **Step 7: Run the native suite**

Run: `make test-native`
Expected: PASS, including the new test and `TestAnHDR10SourceComesOutSDR` unchanged.

- [ ] **Step 8: Commit**

```bash
git add pkg/video/pipeline/eligible.go pkg/video/pipeline/eligible_test.go
git commit -m "video/pipeline: a pod judges the source it opens against an eligibility rule (codec, size, HDR) and answers video.ErrNotForPool before any output; libx264 takes a thread count" -- pkg/video/job.go pkg/video/pipeline/eligible.go pkg/video/pipeline/eligible_test.go pkg/video/pipeline/pipeline.go pkg/video/pipeline/video.go pkg/video/pipeline/filters_test.go
```

---

### Task 2: The worker answers FailedPrecondition, and its flags

**Files:**
- Modify: `pkg/video/worker/server.go` (`Convert`, after `err = s.run(...)`)
- Modify: `cmd/video-worker/main.go` (`run()` flags, `worker.Server.Options`)
- Create: `cmd/video-worker/eligibility.go`, `cmd/video-worker/eligibility_test.go`
- Test: `pkg/video/worker/server_test.go`

**Interfaces:**
- Consumes: `video.ErrNotForPool`, `pipeline.Eligibility`, `pipeline.Options.Threads` (Task 1).
- Produces: a `Convert` stream that, when the pipeline returns `video.ErrNotForPool`, ends with `status.Error(codes.FailedPrecondition, err.Error())` and sends no `Done`; flags `--sources` (comma list, default `""`), `--max-source-pixels` (int, default 0), `--no-hdr` (bool, default false), `--threads` (int, default 0); `func eligibility(sources string, maxPixels int, noHDR bool) *pipeline.Eligibility` returning nil when all three are unset.

- [ ] **Step 1: Write the failing worker test** in `pkg/video/worker/server_test.go`, modelled on `TestAJobBeyondTheSlotsIsResourceExhausted` (a `Server` with a fake `Run`):

```go
func TestASourceBeyondThePoolIsFailedPrecondition(t *testing.T) {
	srv := newServer(t) // the file's helper
	srv.Run = func(context.Context, video.Job, pipeline.Options, pipeline.Sink) error {
		return fmt.Errorf("%w: hevc", video.ErrNotForPool)
	}
	stream := convert(t, srv, hlsJob(t)) // the file's helpers: open Convert, send the job
	_, err := stream.Recv()
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, status.Convert(err).Message(), "not for this pool: hevc")
	assert.Zero(t, srv.Slots.InUse(), "the slot is released") // or the file's equivalent accessor
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./pkg/video/worker/ -run TestASourceBeyondThePoolIsFailedPrecondition`
Expected: FAIL: the stream delivers a `Done` carrying the error instead.

- [ ] **Step 3: Implement** in `Convert`: after `err = s.run(ctx, job, o, out)` and the `errAbandoned` branch, add `if errors.Is(err, video.ErrNotForPool) { log.Info("conversion is not for this pool", "reason", err); return status.Error(codes.FailedPrecondition, err.Error()) }`.

- [ ] **Step 4: Run the worker suite**: `go test ./pkg/video/worker/...` Expected: PASS.

- [ ] **Step 5: Write the failing flag test** in `cmd/video-worker/eligibility_test.go`:

```go
func TestEligibilityComesFromTheFlags(t *testing.T) {
	assert.Nil(t, eligibility("", 0, false), "no flag: every source")
	e := eligibility("h264, mpeg2video", 2073600, true)
	require.NotNil(t, e)
	assert.Equal(t, []string{"h264", "mpeg2video"}, e.Sources)
	assert.Equal(t, 2073600, e.MaxPixels)
	assert.False(t, e.HDR)
	assert.True(t, eligibility("", 0, false) == nil && eligibility("h264", 0, false).HDR, "only --no-hdr refuses HDR")
}
```

- [ ] **Step 6: Run it to make sure it fails**: `go test ./cmd/video-worker/ -run TestEligibilityComesFromTheFlags` Expected: FAIL, `undefined: eligibility`.

- [ ] **Step 7: Implement** `eligibility` in `cmd/video-worker/eligibility.go` (trim and drop empty entries), the four flags in `run()`, and pass `Eligible: eligibility(*sources, *maxPixels, *noHDR), Threads: *threads` into `srv.Options`. Log them on the "serving" line.

- [ ] **Step 8: Run**: `go test ./cmd/video-worker/... ./pkg/video/worker/...` Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add cmd/video-worker/eligibility.go cmd/video-worker/eligibility_test.go
git commit -m "video-worker: a source beyond the pod's rule (--sources, --max-source-pixels, --no-hdr) ends the stream FailedPrecondition before any output, which the relay passes over; --threads bounds libx264" -- pkg/video/worker/server.go pkg/video/worker/server_test.go cmd/video-worker/main.go cmd/video-worker/eligibility.go cmd/video-worker/eligibility_test.go
```

---

### Task 3: Reading clustarr's work Lease

**Files:**
- Create: `pkg/video/lease/work.go`
- Modify: `pkg/video/lease/lease.go` (`Count` generalised)
- Test: `pkg/video/lease/lease_test.go`

**Interfaces:**
- Produces: `func CountOf(l *coordinationv1.Lease, annotation string, now time.Time) int` (the rule `Count` applies, by annotation; `Count` calls it with `Annotation`); `type Work struct{ Client kubernetes.Interface; Namespace, Name, Annotation string; Now func() time.Time }` with `func (w *Work) Busy(ctx context.Context) (bool, error)`: a Get of the Lease, `CountOf(...) > 0`; a NotFound is `(false, nil)`; any other error `(false, err)`. Default `Annotation` `clustarr.io/intel-work`, exported as `WorkAnnotation`.

- [ ] **Step 1: Write the failing test** in `pkg/video/lease/lease_test.go`, with the fake clientset the file's other tests use:

```go
func TestClustarrsWorkIsReadByOurRule(t *testing.T) {
	cs := fake.NewSimpleClientset()
	w := &lease.Work{Client: cs, Namespace: "clustarr-system", Name: "clustarr-intel-work", Annotation: lease.WorkAnnotation, Now: func() time.Time { return t0 }}
	busy, err := w.Busy(t.Context())
	require.NoError(t, err)
	assert.False(t, busy, "no Lease is no work")
	put(t, cs, "clustarr-system", "clustarr-intel-work", map[string]string{lease.WorkAnnotation: "3"}, t0.Add(-5*time.Second)) // the file's Lease-writing helper, or write one
	busy, _ = w.Busy(t.Context())
	assert.True(t, busy)
	put(t, cs, "clustarr-system", "clustarr-intel-work", map[string]string{lease.WorkAnnotation: "3"}, t0.Add(-31*time.Second))
	busy, _ = w.Busy(t.Context())
	assert.False(t, busy, "stale after 30 s")
	put(t, cs, "clustarr-system", "clustarr-intel-work", map[string]string{lease.WorkAnnotation: "0"}, t0)
	busy, _ = w.Busy(t.Context())
	assert.False(t, busy, "zero is idle")
}
```

- [ ] **Step 2: Run it to make sure it fails**: `go test ./pkg/video/lease/ -run TestClustarrsWorkIsReadByOurRule` Expected: FAIL, `undefined: lease.Work`.

- [ ] **Step 3: Implement** `CountOf` and `Work.Busy` in `pkg/video/lease/work.go`; make `Count` a one-line call of `CountOf`.

- [ ] **Step 4: Run the lease suite**: `go test ./pkg/video/lease/...` Expected: PASS, `TestCountReadsZeroWhenStale` unchanged.

- [ ] **Step 5: Commit**

```bash
git add pkg/video/lease/work.go
git commit -m "video/lease: the manager reads clustarr's intel-work Lease by the rule clustarr reads ours (Work.Busy, CountOf)" -- pkg/video/lease/work.go pkg/video/lease/lease.go pkg/video/lease/lease_test.go
```

---

### Task 4: The relay orders two pools and passes one over

**Files:**
- Modify: `pkg/video/relay/relay.go` (`Relay`, `run`, `open`, `relay`)
- Modify: `pkg/video/relay/placements.go` (`Placement.Pool`)
- Create: `pkg/video/relay/pools.go`
- Test: `pkg/video/relay/relay_test.go`, `pkg/video/relay/pools_test.go`

**Interfaces:**
- Consumes: nothing new from other tasks (the fake worker in the tests returns the codes itself).
- Produces: `Relay.CPU remoteexec.WorkerLister` (nil: no CPU pool); `Relay.CPUFirst func(ctx context.Context) bool` (nil: never first); `Relay.Routed func(pool string)`; `Relay.PassedOver func(pool, reason string)` (reasons `full`, `ineligible`); `Placement.Pool string` (`gpu` or `cpu`); `const PoolGPU, PoolCPU = "gpu", "cpu"`; `func cpuEligible(job video.Job) bool` = `job.Output != video.OutputFile && !job.Video.ToneMap`; `func (r *Relay) order(ctx context.Context, job video.Job) []pool` where `type pool struct{ name string; workers remoteexec.WorkerLister }`: the GPU pool alone when `r.CPU == nil` or `!cpuEligible(job)`; else CPU then GPU when `r.CPUFirst(ctx)`, GPU then CPU otherwise. `Count.Begin` runs only for `PoolGPU`.

- [ ] **Step 1: Write the failing order tests** in `pkg/video/relay/pools_test.go` (an internal test, `package relay`):

```go
func TestThePoolsAreOrderedByTheRuleAndTheJob(t *testing.T) {
	gpu, cpu := workers{{Name: "gpu-0"}}, workers{{Name: "cpu-0"}}
	hls := video.Job{Output: video.OutputHLS}
	names := func(ps []pool) []string { ... }
	r := &Relay{Workers: gpu}
	assert.Equal(t, []string{PoolGPU}, names(r.order(t.Context(), hls)), "no CPU pool")
	r = &Relay{Workers: gpu, CPU: cpu, CPUFirst: func(context.Context) bool { return true }}
	assert.Equal(t, []string{PoolCPU, PoolGPU}, names(r.order(t.Context(), hls)))
	r.CPUFirst = func(context.Context) bool { return false }
	assert.Equal(t, []string{PoolGPU, PoolCPU}, names(r.order(t.Context(), hls)), "the CPU pool is the second chance")
	r.CPUFirst = func(context.Context) bool { return true }
	assert.Equal(t, []string{PoolGPU}, names(r.order(t.Context(), video.Job{Output: video.OutputFile})), "a whole-file job never asks the CPU pool")
	assert.Equal(t, []string{PoolGPU}, names(r.order(t.Context(), video.Job{Output: video.OutputHLS, Video: video.VideoOut{ToneMap: true}})), "nor an HDR job")
	r = &Relay{Workers: gpu, CPU: cpu}
	assert.Equal(t, []string{PoolGPU, PoolCPU}, names(r.order(t.Context(), hls)), "nil CPUFirst is never first")
}
```

- [ ] **Step 2: Run it to make sure it fails**: `go test ./pkg/video/relay/ -run TestThePoolsAreOrdered` Expected: FAIL, `undefined: pool`.

- [ ] **Step 3: Implement** `pools.go`: `pool`, `PoolGPU`, `PoolCPU`, `cpuEligible`, `(*Relay).order`.

- [ ] **Step 4: Run it**: PASS.

- [ ] **Step 5: Write the failing relay tests** in `relay_test.go`. Extend `setup` with a second map for CPU workers (`setup2(t, gpu, cpu map[string]*worker, cpuFirst bool)` building `Relay{Workers, CPU, CPUFirst, Routed, PassedOver}` and recording `h.routed []string`, `h.passed []string` as `"cpu:ineligible"`), and give the fake `worker` a `runErr error` returned from `Convert` after the accept header (before `w.run`) when set:

```go
func TestAConversionGoesToTheCPUPoolFirstWhenClustarrIsBusy(t *testing.T) {
	gpu, cpu := &worker{run: script(seg(0, 0, 8, false), done("", ""))}, &worker{run: script(seg(0, 0, 8, false), done("", ""))}
	h := setup2(t, map[string]*worker{"video-0": gpu}, map[string]*worker{"cpu-0": cpu}, true)
	require.NoError(t, h.relay.Execute(t.Context(), h.req, &sink{}))
	assert.NotEmpty(t, cpu.job); assert.Empty(t, gpu.job)
	assert.Zero(t, h.count.begun, "a CPU conversion is not in the GPU Lease")
	assert.Equal(t, []string{relay.PoolCPU}, h.routed)
}

func TestAConversionGoesToTheGPUPoolWhenClustarrIsIdle(t *testing.T) { // cpuFirst false: gpu.job set, cpu.job empty, h.count.begun == 1, routed ["gpu"] }

func TestAFullGPUPoolFallsBackToTheCPUPool(t *testing.T) { // gpu refuse ResourceExhausted, cpuFirst false: cpu serves; h.passed == ["gpu:full"]; begun 0 }

func TestAFullCPUPoolFallsBackToTheGPUPool(t *testing.T) { // cpu refuse ResourceExhausted, cpuFirst true: gpu serves; passed ["cpu:full"]; begun 1 }

func TestASourceTheCPUPoolPassesOverRunsOnTheGPU(t *testing.T) {
	cpu := &worker{runErr: status.Error(codes.FailedPrecondition, "not for this pool: hevc")}
	gpu := &worker{run: script(seg(0, 0, 8, false), done("", ""))}
	h := setup2(t, map[string]*worker{"video-0": gpu}, map[string]*worker{"cpu-0": cpu}, true)
	require.NoError(t, h.relay.Execute(t.Context(), h.req, &sink{}))
	assert.NotEmpty(t, gpu.job)
	assert.Equal(t, []string{"cpu:ineligible"}, h.passed)
	assert.EqualValues(t, 1, h.count.begun)
	p, _ := h.relay.Placements.Get(session(h.req)) ... // the placement, while running, named the GPU pool: assert via a Placements field set in setup2 and read inside the gpu worker's script (as TestTheStartupOverheadIsObserved reads state mid-run), Pool == relay.PoolGPU
}

func TestAWorkerThatDiesAfterAcceptingIsNotPassedOver(t *testing.T) {
	cpu := &worker{runErr: status.Error(codes.Aborted, "the pod is wedged")}
	gpu := &worker{run: script(seg(0, 0, 8, false), done("", ""))}
	h := setup2(t, ..., true)
	s := &sink{}
	require.Error(t, h.relay.Execute(t.Context(), h.req, s))
	assert.Contains(t, s.stderr(), "video: worker-error: ")
	assert.Empty(t, gpu.job, "only FailedPrecondition is a pass-over")
}

func TestAJobNoPoolTakesIsRefusedOnce(t *testing.T) {
	// cpu passes over as ineligible, gpu is full: refused full, one refusal counted, passed ["cpu:ineligible","gpu:full"]
}

func TestAStreamedInputIsPumpedAgainForTheSecondPool(t *testing.T) {
	// as TestAStreamedInputIsReadFromPlexWithTheToken builds a Live TV job: cpu passes over after reading the first chunk,
	// gpu serves; assert the gpu worker's input begins with the same bytes the cpu worker received (both start at the playlist's first segment)
}

func TestARestartIsPlacedAfresh(t *testing.T) {
	// two Executes of the same session (the second with -ss 1800 as TestSegmentsLandBeforeTheirLine's seek fixture), cpuFirst flips between them:
	// after the second run Placements.Get(session) is gone (removed at end) and h.routed == ["cpu","gpu"]
}
```

- [ ] **Step 6: Run them to make sure they fail**: `go test ./pkg/video/relay/ -run 'CPUPool|PassesOver|NotPassedOver|NoPoolTakes|PumpedAgain|PlacedAfresh'` Expected: FAIL (`setup2` undefined, or the GPU-only route).

- [ ] **Step 7: Implement** in `relay.go`:
  - `run`: `full := false; for _, p := range r.order(ctx, job) { ws, err := p.workers.ListReady(ctx) ...; c, err := r.open(ctx, ws, job); switch: refusal full -> full = true, r.passedOver(p.name, "full"), continue; refusal no-pool (no ready pod) -> continue; shape -> return; worker-error -> return }`, then the placement with `Pool: p.name`, `Count.Begin` only if `p.name == PoolGPU`, `r.Routed(p.name)`, `go c.pump(...)`, `err = r.relay(...)`; `if errors.Is(err, errNotForPool) { c.close(); end the count; r.Placements.remove; r.passedOver(p.name, "ineligible"); continue }`; after the loop: `refuse(video.ReasonFull, ...)` when `full`, else `refuse(video.ReasonNoPool, errors.New("no pool takes this conversion"))`.
  - `relay`: on `c.stream.Recv()` error, `if status.Code(err) == codes.FailedPrecondition && !output && !posted { return errNotForPool }` (`var errNotForPool = errors.New("not for this pool")`).
  - `open` is unchanged (a `FailedPrecondition` never arrives before the accept header).
  - `pump` for a streamed input must start from the input's beginning on each pool: it already does (each `open` returns a fresh conversion and `pump` reads `job.Inputs[i].Source` from the start); assert, do not assume, with Step 5's streamed test.

- [ ] **Step 8: Run the relay suite**: `go test ./pkg/video/relay/...` Expected: PASS, the older tests untouched (`TestAFullPoolIsRefusedFull` now sees `passed == ["gpu:full"]` and still `ReasonFull`).

- [ ] **Step 9: Commit**

```bash
git add pkg/video/relay/pools.go pkg/video/relay/pools_test.go
git commit -m "video/relay: two pools, ordered per conversion (CPUFirst) with each the other's second chance; a FailedPrecondition before any output passes the pool over; only a GPU placement is counted in the Lease" -- pkg/video/relay/relay.go pkg/video/relay/placements.go pkg/video/relay/pools.go pkg/video/relay/pools_test.go pkg/video/relay/relay_test.go
```

---

### Task 5: The manager's flags, wiring and metrics

**Files:**
- Modify: `cmd/manager/config.go` (`Config`, `defineFlags`, the viper binding block)
- Modify: `cmd/manager/execserver.go` (the `cfg.VideoSelector != ""` block)
- Modify: `pkg/telemetry/telemetry.go` (two counters)
- Test: `cmd/manager/config_test.go`, `cmd/manager/execserver_test.go`

**Interfaces:**
- Consumes: `lease.Work` (Task 3), `Relay.CPU/CPUFirst/Routed/PassedOver` (Task 4).
- Produces: `Config.VideoCPUSelector string` (flag `video-cpu-selector`, default `""`: no CPU pool), `Config.VideoCPUWhen string` (`video-cpu-when`, default `clustarr-busy`), `Config.ClustarrWorkLease string` (`clustarr-work-lease`, `<namespace>/<name>`, default `""`); `func (c Config) cpuFirst(k kubernetes.Interface, log *slog.Logger) func(context.Context) bool` (`always` -> true, `never` -> false, `clustarr-busy` -> `lease.Work.Busy`, an error logged once per job and read as false; an empty `ClustarrWorkLease` under `clustarr-busy` is false); `Metrics.VideoConversions *prometheus.CounterVec` (`clusterplex_video_conversions_total{pool}`), `Metrics.VideoPassedOver *prometheus.CounterVec` (`clusterplex_video_pool_passed_over_total{pool,reason}`).

- [ ] **Step 1: Write the failing config test** in `cmd/manager/config_test.go`, beside the file's flag tests:

```go
func TestTheCPUPoolFlagsHaveTheirDefaultsAndBounds(t *testing.T) {
	cfg := load(t) // the file's helper that parses defaults
	assert.Equal(t, "", cfg.VideoCPUSelector)
	assert.Equal(t, "clustarr-busy", cfg.VideoCPUWhen)
	assert.Equal(t, "", cfg.ClustarrWorkLease)
	_, err := loadWith(t, "--video-cpu-when=sometimes")
	require.ErrorContains(t, err, "video-cpu-when")
	cfg, err = loadWith(t, "--video-cpu-selector=app=plex-video-transcode-cpu", "--video-cpu-when=always", "--clustarr-work-lease=clustarr-system/clustarr-intel-work")
	require.NoError(t, err)
	assert.Equal(t, "app=plex-video-transcode-cpu", cfg.VideoCPUSelector)
}
```

- [ ] **Step 2: Run it to make sure it fails**: `go test ./cmd/manager/ -run TestTheCPUPoolFlags` Expected: FAIL.

- [ ] **Step 3: Implement** the three fields, flags and validation (`video-cpu-when` must be one of `clustarr-busy`, `always`, `never`; `clustarr-work-lease` must be `<namespace>/<name>` when set).

- [ ] **Step 4: Write the failing wiring test** in `cmd/manager/execserver_test.go`, beside `TestTheVideoRouteIsWiredWhenASelectorIsSet`:

```go
func TestTheCPUPoolIsWiredWhenItsSelectorIsSet(t *testing.T) {
	m := videoManager(t) // whatever TestTheVideoRouteIsWiredWhenASelectorIsSet builds
	m.Config.VideoSelector, m.Config.VideoLeaseName = "app=plex-video-transcode", "cluster-plex-video-transcodes"
	m.Config.VideoCPUSelector, m.Config.VideoCPUWhen = "app=plex-video-transcode-cpu", "always"
	remote, _, err := m.jobServices()
	require.NoError(t, err)
	r := remote.Execute ... // reach the *videorelay.Relay as the existing test does
	assert.NotNil(t, r.CPU)
	assert.True(t, r.CPUFirst(t.Context()), "always")
	m.Config.VideoCPUSelector = ""
	// rebuilt: r.CPU nil
}
```

- [ ] **Step 5: Implement** the wiring in `execserver.go`: `CPU: &remoteexec.PodWorkerLister{... Selector: cfg.VideoCPUSelector, Port: cfg.VideoPort}` when set, `CPUFirst: cfg.cpuFirst(m.K8sClient, log)`, `Routed: func(p string) { m.Metrics.VideoConversions.WithLabelValues(p).Inc() }`, `PassedOver: func(p, why string) { m.Metrics.VideoPassedOver.WithLabelValues(p, why).Inc() }`; add the two counters to `pkg/telemetry/telemetry.go` with the Help texts `"Video conversions started, by the pool that took them (gpu, cpu)"` and `"A pool passed over for a conversion, by pool and reason (full, ineligible)"`. Log the pool on the "video conversion started" line (`Placement.Pool`).

- [ ] **Step 6: Run**: `go test ./cmd/manager/... ./pkg/telemetry/...` Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git commit -m "manager: --video-cpu-selector, --video-cpu-when (clustarr-busy|always|never) and --clustarr-work-lease wire the CPU pool into the video route; conversions and pass-overs are counted by pool" -- cmd/manager/config.go cmd/manager/config_test.go cmd/manager/execserver.go cmd/manager/execserver_test.go pkg/telemetry/telemetry.go
```

---

### Task 6: The chart, the docs and the as-built record

**Files:**
- Modify: `charts/cluster-plex/values.yaml` (`videoTranscode.cpu`), `charts/cluster-plex/Chart.yaml` (version)
- Create: `charts/cluster-plex/templates/video-transcode-cpu.yaml`
- Modify: `charts/cluster-plex/templates/storage.yaml` (the manager config keys), `charts/cluster-plex/templates/rbac.yaml` (the `clustarr-reader` Role)
- Modify: `docs/configuration.md` (the video pool's flag and value tables), `docs/adr/0010-video-conversions-on-the-gpu-pool.md` (an amendment), `docs/superpowers/specs/2026-11-10-cpu-video-route-design.md` ("As built")

**Interfaces:**
- Consumes: the flags of Tasks 2 and 5.
- Produces: values `videoTranscode.cpu.enabled` (false), `replicas` (1), `maxJobs` (2), `resources` (requests cpu `"3"` memory `2Gi`, limits cpu `"8"` memory `4Gi`), `nodeSelector` (`{}`), `affinity` (a required `kubernetes.io/hostname` term the installer fills; the default renders no affinity and a NOTES line), `tolerations` ([]), `when` (`clustarr-busy`), `sources` (`[h264, mpeg2video]`), `maxSourcePixels` (2073600), `hdr` (false), `threads` (8), `clustarrWorkLease` (`""`: the chart derives `<plex.clustarr.namespace>/clustarr-intel-work` when `plex.clustarr.enabled`, else none); a Deployment `<fullname>-video-transcode-cpu` with labels `app: plex-video-transcode-cpu`, the GPU pool's security context, inputs emptyDir and media mount, no `gpu.intel.com/i915`, args `--max-jobs`, `--encoder=libx264`, `--inputs-dir=/run/video-inputs`, `--sources=<joined>`, `--max-source-pixels`, `--no-hdr` (when `hdr` false), `--threads`; manager config keys `video-cpu-selector: "app=plex-video-transcode-cpu"`, `video-cpu-when`, `clustarr-work-lease` rendered only when `cpu.enabled`; the `clustarr-reader` Role gains `get` on `leases` with `resourceNames: [clustarr-intel-work]`.

- [ ] **Step 1: Write the values and the template**; copy the GPU Deployment's pod security context, probes and mounts, drop the GPU resource and render node, add the flags.

- [ ] **Step 2: Render and check**

Run: `helm template cluster-plex charts/cluster-plex --set postgres.host=postgres --set postgres.passwordSecret.name=plex-postgres --set videoTranscode.enabled=true --set videoTranscode.cpu.enabled=true --set plex.clustarr.enabled=true | grep -E 'video-transcode-cpu|--encoder=libx264|--sources=|--no-hdr|--threads=|video-cpu-selector|video-cpu-when|clustarr-work-lease|clustarr-intel-work'`
Expected: the Deployment name, `--encoder=libx264`, `--sources=h264,mpeg2video`, `--no-hdr`, `--threads=8`, `video-cpu-selector: "app=plex-video-transcode-cpu"`, `video-cpu-when: "clustarr-busy"`, `clustarr-work-lease: "<ns>/clustarr-intel-work"`, and the Role's `resourceNames`. Without `cpu.enabled`: none of them.

Run: `make helm-lint` Expected: clean. Run `go test ./test/manifests/...` Expected: PASS (the parity tests cover the Plex pods, which did not change).

- [ ] **Step 3: Docs**: `docs/configuration.md`: add the three flags to the manager's table and a `videoTranscode.cpu.*` table under the pool's values, with the "when" semantics in one paragraph (CPU first while clustarr's Lease is fresh and above zero; each pool the other's second chance; whole-file and HDR jobs GPU-only; the worker judges the source). `docs/adr/0010`: an "Amended 2026-11-10" paragraph: the CPU pool as a second pool; Plex's node still runs nothing. The design's "As built" section: what shipped, the commits, anything a task changed from the design.

- [ ] **Step 4: Bump `charts/cluster-plex/Chart.yaml` version** (the next patch above the current, which the Makefile's rule explains); `make helm-lint` again.

- [ ] **Step 5: Lint and the whole plain suite**

Run: `make lint && go test ./...` Expected: clean, PASS (the native suite ran in Task 1).

- [ ] **Step 6: Commit**

```bash
git add charts/cluster-plex/templates/video-transcode-cpu.yaml
git commit -m "chart, docs: videoTranscode.cpu renders the libx264 pool on the NUC and the manager's CPU-route flags; configuration.md, ADR-0010 amended, the design's as-built record" -- charts/cluster-plex/values.yaml charts/cluster-plex/Chart.yaml charts/cluster-plex/templates/video-transcode-cpu.yaml charts/cluster-plex/templates/storage.yaml charts/cluster-plex/templates/rbac.yaml docs/configuration.md docs/adr/0010-video-conversions-on-the-gpu-pool.md docs/superpowers/specs/2026-11-10-cpu-video-route-design.md
```

---

### Task 7: Proof on talos (after both halves ship)

Not code: the spec's "Verifying it" steps 1-4, run by whoever deploys (the chart that pulls this cluster-plex, `cpu.enabled: true`, `affinity` on `talos-88-ae-dd-75-3d-a0`, `when: always` first, then `clustarr-busy` once clustarr's Lease exists). Each step's expected reading is in the spec; record the placements (`clusterplex_video_conversions_total{pool}`), the worker's `clusterplex_video_worker_encode_speed` on the CPU pod (every bucket at or above 1.0 for a stream), and clustarr's `clustarr_transcode_jobs_active{class="intel"}` at two beside a CPU conversion, in the design's "As built".
