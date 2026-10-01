# Browser Remux Transcoder Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Plex Web's DASH streams (video copied, audio to stereo AAC) run on a cluster-plex remux pool built on ffgo in process, with a per-worker remux cache, instead of on Plex's own transcoder.

**Architecture:** The manager classifies each shimmed `Plex Transcoder` job; a browser remux is parsed into a typed `remux.Job` and streamed over gRPC to a remux worker picked by consistent hash. The worker demuxes once with ffgo, cuts segments at deterministic keyframe boundaries, muxes each representation into fragmented MP4 through `io.Writer`s, and sends every segment both to the client stream and to its local cache. The serving pod's manager writes the segments into Plex's session directory and relays the manifest and progress to its own Plex, in that order.

**Tech Stack:** Go 1.27, ffgo (mediactl fork, FFmpeg 9 through purego), gRPC/protobuf, Kubernetes (kustomize + Helm), FFmpeg 9 (BtbN shared build) in the worker image.

**Spec:** `docs/superpowers/specs/2026-10-01-browser-remux-transcoder-design.md`
(Task 0 amends two points of it; read the amended version.)

## Global Constraints

- Two repositories: `/home/appkins/src/mediactl/ffgo` (module `github.com/obinnaokechukwu/ffgo`, the mediactl fork) and `/home/appkins/src/mediactl/cluster-plex` (module `github.com/mediactl/clusterplex`). Other sessions commit in both: commit with a pathspec (`git commit -m '…' -- <paths>`), never `git stash`, never push without the owner's go-ahead.
- Only `cmd/remux-worker` and the packages it alone imports (`pkg/remux/pipeline`, `pkg/remux/worker`) link ffgo. `cmd/manager`, `cmd/shim`, `cmd/proxy` and `cmd/maintenance` must not (a guard test, Task 10).
- Plex's names are fixed: `init-stream$RepresentationID$.m4s`, `chunk-stream$RepresentationID$-$Number%05d$.m4s`; representation 0 is video, 1 is audio.
- Segment boundary rule: a fresh job starting at segment `n` starts at the first video keyframe at or after `(n−1) × segment duration`; each later segment `k` starts at the first keyframe at or after `(k−1) × segment duration` that is later than segment `k−1`'s start. Timestamps stay in source time (minus the input's start time).
- Fragmented MP4 flags: `movflags=frag_custom+empty_moov+default_base_moof`.
- Anything the classifier, the parser or the worker refuses runs on Plex's transcoder exactly as before; nothing here may make a playback fail that worked before.
- The job's `X_PLEX_TOKEN` (from the shim's environment) is sent as `X-Plex-Token` on every manifest POST and progress PUT; it is never logged.
- Lint and tests per repo: ffgo `go test ./...`; cluster-plex `make test` and `make lint`.

## Review Focus

1. **A keyframe interval longer than the segment duration** (x265's default is 250 frames, ~10 s at 24 fps; much of the owner's library). Segment numbers must still be contiguous, and a seek to a segment inside a long GOP must start at the boundary the rule gives, not at the requested time. Pinned in Task 8 (`TestLongGOPSegmentsAreContiguous`).
2. **Audio packets demuxed ahead of the video keyframe that ends their segment.** The audio muxer must wait for the boundary, never cut early or deadlock the demuxer. Pinned in Task 8 (`TestAudioFragmentsEndAtTheVideoBoundaries`).
3. **A Plex argv with any option the parser does not know** (a new Plex release, a subtitle burn-in, `-ac 6`): refused, so Plex runs it. Pinned in Task 2 (`TestAnUnknownOptionIsRefused`).
4. **A worker that dies mid-stream.** The manager must not report success, and must not leave Plex with a manifest listing segments it never wrote. Pinned in Task 10 (`TestAFailedWorkerAfterOutputExitsNonZero`).
5. **A cached entry whose init differs from a new run's** (an FFmpeg upgrade changes the AAC config): the entry is reset, never mixed. Pinned in Task 5 (`TestADifferentInitResetsTheEntry`).

---

### Task 0: Amend the spec

**Files:**
- Modify: `docs/superpowers/specs/2026-10-01-browser-remux-transcoder-design.md`

Two points of the spec are changed here, before any code:
- progress is relayed through the manager (the worker never reaches Plex and never holds its token);
- the cache tee is per segment, and segments are the same across runs when their boundaries are, not byte-identical in their audio.

- [ ] **Step 1: Replace the progress sentence in §3**

Replace:
```
Progress PUTs go from the worker straight to the serving pod's PMS, at the
address the dispatcher already rewrites `127.0.0.1` to. With no ready
remux worker the job goes to Plex's transcoder: Plex pods carry no
FFmpeg 9.
```
with:
```
Progress is an event on the same stream (`Progress{path, query}`), and the
manager PUTs it to its own PMS, as it does the manifest: the worker never
reaches Plex and never holds the session's token, which the manager sends
as `X-Plex-Token` from the job's `X_PLEX_TOKEN`. With no ready remux worker
the job goes to Plex's transcoder: Plex pods carry no FFmpeg 9.
```

- [ ] **Step 2: Replace the boundary and tee bullets in §2**

Replace the bullet starting `- **Segment boundaries are ours and deterministic.**` with:
```
- **Segment boundaries are ours and deterministic.** A job starting at
  segment *n* starts at the first video keyframe at or after
  `(n−1) × seg_duration`; each later segment *k* starts at the first
  keyframe at or after `(k−1) × seg_duration` that is later than segment
  *k−1*'s start, so numbers stay contiguous when a keyframe interval is
  longer than a segment. Timestamps keep source time (`-copyts`). A
  segment's video is the same in every run that gives it the same start;
  its audio is re-encoded per run and decodes the same.
```
Replace the bullet starting `- **Tee.**` with:
```
- **Tee.** Every segment the splitter cuts goes to the client stream and
  to the cache in the same step; one demux and one audio encode feed both.
```
And in §4 replace `appends as well; the index finds segments by number wherever they lie.` with `appends as well; the index finds segments by number wherever they lie, and records each segment's start and end so a chain is served from the cache only while each segment starts where the previous one ended.`

- [ ] **Step 3: Commit**

```bash
cd /home/appkins/src/mediactl/cluster-plex
git commit -m "docs(spec): progress is relayed by the manager; the cache tee is per segment and chains by boundary" -- docs/superpowers/specs/2026-10-01-browser-remux-transcoder-design.md
```

---

### Task 1: ffgo — a Muxer that writes to an io.Writer, Flush, OutputTimeBase

**Files (in `/home/appkins/src/mediactl/ffgo`):**
- Modify: `avformat/avformat.go` (bind `avio_flush`)
- Modify: `muxer.go`
- Create: `muxer_writer_test.go`

**Interfaces:**
- Produces: `func NewMuxerToWriter(w io.Writer, format string) (*Muxer, error)`; `func (m *Muxer) Flush() error`; `func (ms *MuxerStream) OutputTimeBase() Rational`; `func avformat.IOFlush(ctx avformat.IOContext)`.

- [ ] **Step 1: Write the failing test** — `muxer_writer_test.go`:

```go
//go:build !ios && !android && (amd64 || arm64)

package ffgo

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// A writer-backed mp4 muxer with frag_custom writes one moof per Flush, and
// what it wrote is an MP4 ffprobe decodes whole. This is what the remux
// worker cuts DASH segments from.
func TestAWriterMuxerEmitsOneFragmentPerFlush(t *testing.T) {
	clip := h264Clip(t, 72) // 3 s at 24 fps
	d, err := NewDecoder(clip)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var src *StreamInfo
	for _, s := range d.Streams() {
		if s.Type == MediaTypeVideo {
			src = s
		}
	}
	var out bytes.Buffer
	m, err := NewMuxerToWriter(&out, "mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ms, err := m.AddCopyStream(&CopyStreamConfig{CodecParameters: src.CodecParameters(), TimeBase: src.TimeBase})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteHeaderWithOptions(map[string]string{"movflags": "frag_custom+empty_moov+default_base_moof"}); err != nil {
		t.Fatal(err)
	}
	if tb := ms.OutputTimeBase(); tb.Num == 0 || tb.Den == 0 {
		t.Fatalf("output time base after the header = %v, want the muxer's", tb)
	}
	if err := m.Flush(); err != nil { // the init reaches the writer
		t.Fatal(err)
	}
	if got := countBoxes(out.Bytes(), "moov"); got != 1 {
		t.Fatalf("after the header and a Flush: %d moov, want 1", got)
	}
	flushes, n := 0, 0
	for {
		p, err := d.ReadPacket()
		if err != nil {
			t.Fatal(err)
		}
		if p == nil {
			break
		}
		if p.StreamIndex() != src.Index {
			continue
		}
		if n > 0 && n%24 == 0 {
			if err := m.Flush(); err != nil {
				t.Fatal(err)
			}
			flushes++
		}
		n++
		c, err := p.Clone()
		if err != nil {
			t.Fatal(err)
		}
		if err := m.WritePacket(ms, c); err != nil {
			t.Fatal(err)
		}
		_ = c.Free()
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	flushes++
	if err := m.WriteTrailer(); err != nil {
		t.Fatal(err)
	}
	if got := countBoxes(out.Bytes(), "moof"); got != flushes {
		t.Fatalf("moof boxes = %d, want one per Flush (%d)", got, flushes)
	}
	path := filepath.Join(t.TempDir(), "out.mp4")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ffprobeFrameCount(t, path, 0); got != 72 {
		t.Fatalf("frames decoded = %d, want 72", got)
	}
}

// countBoxes counts the top-level ISO BMFF boxes of type typ in b.
func countBoxes(b []byte, typ string) int {
	n := 0
	for len(b) >= 8 {
		size, hdr := int(binary.BigEndian.Uint32(b)), 8
		if size == 1 && len(b) >= 16 {
			size, hdr = int(binary.BigEndian.Uint64(b[8:])), 16
		}
		if size < hdr || size > len(b) {
			break
		}
		if string(b[4:8]) == typ {
			n++
		}
		b = b[size:]
	}
	return n
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd /home/appkins/src/mediactl/ffgo && go test -count=1 -run TestAWriterMuxerEmitsOneFragmentPerFlush .`
Expected: FAIL to build: `undefined: NewMuxerToWriter` (and `m.Flush`, `ms.OutputTimeBase`).

- [ ] **Step 3: Bind `avio_flush`** — in `avformat/avformat.go`, beside `avioClosep` in the `var (...)` block add:

```go
	avioFlush func(ctx uintptr)
```
beside `purego.RegisterLibFunc(&avioClosep, …)` add:
```go
	purego.RegisterLibFunc(&avioFlush, lib, "avio_flush")
```
and after `IOCloseP`:
```go
// IOFlush forces the bytes buffered in ctx out to its writer or file.
func IOFlush(ctx IOContext) {
	if ctx == nil || avioFlush == nil {
		return
	}
	avioFlush(uintptr(ctx))
}
```

- [ ] **Step 4: Implement the writer muxer** — in `muxer.go`:

Add to `Muxer` the field `customIO *CustomIOContext // set by NewMuxerToWriter; the muxer owns it`. Add after `NewMuxer`:

```go
// NewMuxerToWriter creates a muxer writing to w. w is never seeked, so a
// format that rewrites its header on close (plain MP4) must be made
// streamable through its options, e.g. movflags
// frag_custom+empty_moov+default_base_moof, passed to WriteHeaderWithOptions.
func NewMuxerToWriter(w io.Writer, format string) (*Muxer, error) {
	if err := Init(); err != nil {
		return nil, err
	}
	if w == nil {
		return nil, errors.New("ffgo: writer cannot be nil")
	}
	if format == "" {
		return nil, errors.New("ffgo: a writer muxer needs a format")
	}
	cio, err := NewCustomIOContext(&IOCallbacks{Write: w.Write}, true)
	if err != nil {
		return nil, err
	}
	m := &Muxer{customIO: cio, streams: make([]*MuxerStream, 0)}
	if err := avformat.AllocOutputContext2(&m.formatCtx, nil, format, ""); err != nil {
		_ = cio.Close()
		return nil, err
	}
	return m, nil
}
```

In `writeHeaderLocked`, replace the `IOOpen` + `SetIOContext` lines with:
```go
	if m.customIO != nil {
		avformat.SetIOContext(m.formatCtx, m.customIO.AVIOContext())
	} else {
		if err := avformat.IOOpen(&m.ioCtx, m.path, avformat.IOFlagWrite); err != nil {
			return err
		}
		avformat.SetIOContext(m.formatCtx, m.ioCtx)
	}
```

Add after `WriteTrailer`:
```go
// Flush ends the fragment being written and pushes every buffered byte to
// the output: av_write_frame(ctx, NULL), which with movflags=frag_custom
// makes the mp4 muxer write a fragment of what it holds, then avio_flush.
// Packets still waiting in av_interleaved_write_frame's queue are not part
// of it; with one stream per muxer none wait.
func (m *Muxer) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("ffgo: muxer is closed")
	}
	if !m.headerWritten {
		return errors.New("ffgo: header not written")
	}
	if err := avformat.WriteFrame(m.formatCtx, nil); err != nil {
		return err
	}
	if m.customIO != nil {
		avformat.IOFlush(m.customIO.AVIOContext())
	} else {
		avformat.IOFlush(m.ioCtx)
	}
	return nil
}
```

In `Close`, after freeing the format context add:
```go
	if m.customIO != nil {
		_ = m.customIO.Close()
		m.customIO = nil
	}
```

Add after `TimeBase()`:
```go
// OutputTimeBase is the stream's time base in the written container, which
// the muxer chooses in WriteHeader (mp4: 1/sample rate for audio, a
// multiple of the frame rate for video). Zero before the header.
func (ms *MuxerStream) OutputTimeBase() Rational {
	num, den := avformat.GetStreamTimeBase(ms.stream)
	return NewRational(num, den)
}
```
Add `"io"` to `muxer.go`'s imports.

- [ ] **Step 5: Run the test and the package**

Run: `go test -count=1 -run TestAWriterMuxerEmitsOneFragmentPerFlush . && go test -count=1 ./...`
Expected: PASS (both).

- [ ] **Step 6: Commit and tag**

```bash
git commit -m "feat: NewMuxerToWriter, Muxer.Flush and MuxerStream.OutputTimeBase -- a fragmented MP4 muxed into an io.Writer, one moof per Flush, for cluster-plex's remux worker" -- avformat/avformat.go muxer.go muxer_writer_test.go
git tag v0.0.0-clustarr.10
```
**Stop and ask the owner before `git push origin main v0.0.0-clustarr.10`.** cluster-plex's Docker build needs the tag on GitHub; local work may proceed meanwhile with the `replace` in Task 2 Step 1.

---

### Task 2: `pkg/remux` — the Job and Plex's argv parser

**Files (cluster-plex from here on):**
- Create: `pkg/remux/job.go`, `pkg/remux/parse.go`
- Create: `pkg/remux/parse_test.go`, `pkg/remux/words_test.go`
- Fixtures (already in the tree, uncommitted): `pkg/remux/testdata/plex-dash-jobs.log` (44 real DASH jobs from plex-0's log, token redacted by Plex), `pkg/remux/testdata/plex-static.mpd` (the final manifest Plex's binary posted for a 30 s clip)
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Produces:
```go
var ErrNotRemux = errors.New("not a browser remux")
type Job struct {
	Input           string
	Start           time.Duration // -ss; 0 from the beginning
	SkipToSegment   int           // ≥ 1
	SegmentDuration time.Duration
	VideoStream     int // input stream index, copied
	AudioStream     int // input stream index
	AudioCopy       bool
	AudioChannels   int   // output channels; 0 keeps the source's
	AudioSampleRate int   // 0 keeps the source's
	AudioBitRate    int64 // 0 when copied
	AudioLanguage   string
	ProgressURL     string
	ManifestURL     string
	Token           string // X_PLEX_TOKEN; never logged
}
func Parse(args []string, env map[string]string) (Job, error)
func (j Job) SessionID() string
func (j Job) RingKey() string
```

- [ ] **Step 1: Add ffgo to the module**

```bash
cd /home/appkins/src/mediactl/cluster-plex
go mod edit -require=github.com/obinnaokechukwu/ffgo@v0.0.0-00010101000000-000000000000
go mod edit -replace=github.com/obinnaokechukwu/ffgo=../ffgo
```
(Until the owner pushes the tag. Once pushed: `go mod edit -replace=github.com/obinnaokechukwu/ffgo=github.com/mediactl/ffgo@v0.0.0-clustarr.10 && go mod tidy`. Task 11's image build requires the pushed form.) No package imports ffgo yet; `go mod tidy` runs in Task 8.

- [ ] **Step 2: Write the test helper** — `pkg/remux/words_test.go`:

```go
package remux

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// words splits a command line as Plex logs it: single quotes literal,
// double quotes with backslash escapes, a backslash escaping one byte.
func words(line string) []string {
	var out []string
	var cur strings.Builder
	in, quote := false, byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			if c == '\\' && i+1 < len(line) {
				i++
				cur.WriteByte(line[i])
			} else if c == '"' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
			in = true
		case c == ' ':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// loggedJob is one line of plex-dash-jobs.log as the shim would send it:
// the leading VAR=value words as the environment, the binary dropped.
func loggedJob(line string) (args []string, env map[string]string) {
	env = map[string]string{}
	w := words(line)
	for len(w) > 0 && strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "-") && !strings.HasPrefix(w[0], "/") {
		k, v, _ := strings.Cut(w[0], "=")
		env[k] = v
		w = w[1:]
	}
	return w[1:], env // w[0] is "/usr/lib/plexmediaserver/Plex Transcoder"
}

func loggedJobs(t *testing.T) [][]string {
	t.Helper()
	f, err := os.Open("testdata/plex-dash-jobs.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]string
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		args, _ := loggedJob(s.Text())
		out = append(out, args)
	}
	if len(out) != 44 {
		t.Fatalf("fixture has %d jobs, want 44", len(out))
	}
	return out
}
```

- [ ] **Step 3: Write the failing tests** — `pkg/remux/parse_test.go`:

```go
package remux

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const arcane = `FFMPEG_EXTERNAL_LIBS='/c/' X_PLEX_TOKEN=tok "/usr/lib/plexmediaserver/Plex Transcoder" -codec:0 hevc -codec:1 aac -ss 495 -noaccurate_seek -analyzeduration 20000000 -probesize 20000000 -i "/library/tv/Arcane (2021) {tvdb-371028}/Season 01/Arcane - S01E07.mkv" -start_at_zero -copyts -fps_mode cfr -y -nostats -loglevel quiet -loglevel_plex error -progressurl http://127.0.0.1:32400/video/:/transcode/session/f168tc7vl2desp78pov5ktwu/b12394b4-a41e-475d-bf98-4a9e52ad87c5/progress -map 0:0 -codec:0 copy -filter_complex "[0:1] aresample=async=1:ochl='stereo':rematrix_maxval=0.000000dB:osr=96000[0]" -map "[0]" -metadata:s:1 language=eng -codec:1 aac -b:1 256k -f dash -seg_duration 5 -dash_segment_type mp4 -init_seg_name 'init-stream$RepresentationID$.m4s' -media_seg_name 'chunk-stream$RepresentationID$-$Number%05d$.m4s' -window_size 5 -delete_removed false -skip_to_segment 100 -manifest_name "http://127.0.0.1:32400/video/:/transcode/session/f168tc7vl2desp78pov5ktwu/b12394b4-a41e-475d-bf98-4a9e52ad87c5/manifest?X-Plex-Http-Pipeline=infinite" -avoid_negative_ts disabled -map_metadata -1 -map_chapters -1 dash`

func TestParseReadsABrowserRemux(t *testing.T) {
	args, env := loggedJob(arcane)
	got, err := Parse(args, env)
	require.NoError(t, err)
	assert.Equal(t, Job{
		Input:           "/library/tv/Arcane (2021) {tvdb-371028}/Season 01/Arcane - S01E07.mkv",
		Start:           495 * time.Second,
		SkipToSegment:   100,
		SegmentDuration: 5 * time.Second,
		VideoStream:     0,
		AudioStream:     1,
		AudioChannels:   2,
		AudioSampleRate: 96000,
		AudioBitRate:    256000,
		AudioLanguage:   "eng",
		ProgressURL:     "http://127.0.0.1:32400/video/:/transcode/session/f168tc7vl2desp78pov5ktwu/b12394b4-a41e-475d-bf98-4a9e52ad87c5/progress",
		ManifestURL:     "http://127.0.0.1:32400/video/:/transcode/session/f168tc7vl2desp78pov5ktwu/b12394b4-a41e-475d-bf98-4a9e52ad87c5/manifest?X-Plex-Http-Pipeline=infinite",
		Token:           "tok",
	}, got)
	assert.Equal(t, "f168tc7vl2desp78pov5ktwu", got.SessionID())
}

// Every DASH job plex-0 ran on 2026-10-01 is one this worker can take.
func TestEveryLoggedBrowserJobParses(t *testing.T) {
	for i, args := range loggedJobs(t) {
		j, err := Parse(args, nil)
		require.NoError(t, err, "job %d", i)
		assert.Equal(t, 5*time.Second, j.SegmentDuration, "job %d", i)
		assert.GreaterOrEqual(t, j.SkipToSegment, 1, "job %d", i)
		assert.NotEmpty(t, j.Input, "job %d", i)
	}
}

func TestAnUnknownOptionIsRefused(t *testing.T) {
	base, env := loggedJob(arcane)
	for name, mutate := range map[string]func([]string) []string{
		"a video encode": func(a []string) []string {
			i := slices.Index(a, "copy")
			a[i] = "libx264"
			return a
		},
		"HLS output": func(a []string) []string {
			i := slices.Index(a, "dash")
			a[i] = "segment"
			return a
		},
		"a subtitle burn-in": func(a []string) []string {
			i := slices.Index(a, "-filter_complex")
			a[i+1] = "[0:0][0:3]overlay[0]"
			return a
		},
		"an option this parser does not know": func(a []string) []string {
			i := slices.Index(a, "-y")
			return slices.Insert(a, i, "-ac", "6")
		},
		"a third output": func(a []string) []string {
			i := slices.Index(a, "-f")
			return slices.Insert(a, i, "-map", "0:2", "-codec:2", "webvtt")
		},
		"other segment names": func(a []string) []string {
			i := slices.Index(a, "-media_seg_name")
			a[i+1] = "seg-$Number$.m4s"
			return a
		},
	} {
		_, err := Parse(mutate(slices.Clone(base)), env)
		assert.True(t, errors.Is(err, ErrNotRemux), "%s: %v", name, err)
	}
}

func TestACopiedAudioTrackIsARemux(t *testing.T) {
	args, env := loggedJob(arcane)
	i := slices.Index(args, "-filter_complex")
	args = slices.Delete(args, i, i+2)
	m := slices.Index(args, `[0]`)
	args[m] = "0:1"
	c := slices.Index(args, "-codec:1")
	args[c+1] = "copy"
	b := slices.Index(args, "-b:1")
	args = slices.Delete(args, b, b+2)
	got, err := Parse(args, env)
	require.NoError(t, err)
	assert.True(t, got.AudioCopy)
	assert.Equal(t, 1, got.AudioStream)
	assert.Zero(t, got.AudioBitRate)
}
```

- [ ] **Step 4: Run to verify they fail**

Run: `go test ./pkg/remux/`
Expected: FAIL to build: `undefined: Parse`, `undefined: Job`.

- [ ] **Step 5: Implement** — `pkg/remux/job.go`:

```go
// Package remux runs Plex Web's DASH streams -- a copy of the video and the
// audio converted to stereo AAC -- on cluster-plex's remux pool instead of
// Plex's transcoder (docs/superpowers/specs/2026-10-01-browser-remux-transcoder-design.md).
// This package is what the manager links: it never imports ffgo.
package remux

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ErrNotRemux marks a job this package will not run, so Plex's transcoder does.
var ErrNotRemux = errors.New("not a browser remux")

// Plex's segment names, which PMS serves from the session directory.
const (
	InitName  = "init-stream$RepresentationID$.m4s"
	MediaName = "chunk-stream$RepresentationID$-$Number%05d$.m4s"
)

// InitFile and ChunkFile are the names of representation rep's files.
func InitFile(rep int) string { return fmt.Sprintf("init-stream%d.m4s", rep) }
func ChunkFile(rep, n int) string { return fmt.Sprintf("chunk-stream%d-%05d.m4s", rep, n) }

// Job is a browser remux as Plex asked for it.
type Job struct {
	Input           string
	Start           time.Duration
	SkipToSegment   int
	SegmentDuration time.Duration
	VideoStream     int
	AudioStream     int
	AudioCopy       bool
	AudioChannels   int
	AudioSampleRate int
	AudioBitRate    int64
	AudioLanguage   string
	ProgressURL     string
	ManifestURL     string
	Token           string
}

// SessionID is Plex's transcode session id, from the progress URL's
// /video/:/transcode/session/<id>/<uuid>/progress.
func (j Job) SessionID() string {
	u, err := url.Parse(j.ProgressURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if p == "session" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// RingKey picks the worker: the same file with the same audio returns to
// the worker holding its cache.
func (j Job) RingKey() string {
	return fmt.Sprintf("%s|%d|%t|%d|%d|%d|%s", j.Input, j.AudioStream, j.AudioCopy,
		j.AudioChannels, j.AudioSampleRate, j.AudioBitRate, j.SegmentDuration)
}
```

`pkg/remux/parse.go`:

```go
package remux

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// downmix is the only audio filter Plex Web's jobs carry: input stream N
// resampled to stereo at osr, output label [0].
var downmix = regexp.MustCompile(`^\[0:(\d+)\] aresample=async=1:ochl='stereo':rematrix_maxval=0\.000000dB:osr=(\d+)\[0\]$`)

// streamRef reads "0:N", the only input Plex maps from.
var streamRef = regexp.MustCompile(`^0:(\d+)$`)

// valueOptions take one argument this parser reads or knowingly ignores;
// flagOptions take none. Anything else is refused, so a Plex release that
// adds an option runs on Plex's transcoder rather than on a guess.
var valueOptions = map[string]bool{
	"-codec:0": true, "-codec:1": true, "-analyzeduration": true, "-probesize": true,
	"-ss": true, "-i": true, "-fps_mode": true, "-loglevel": true, "-loglevel_plex": true,
	"-progressurl": true, "-map": true, "-filter_complex": true, "-metadata:s:1": true,
	"-b:1": true, "-f": true, "-seg_duration": true, "-dash_segment_type": true,
	"-init_seg_name": true, "-media_seg_name": true, "-window_size": true,
	"-delete_removed": true, "-skip_to_segment": true, "-manifest_name": true,
	"-avoid_negative_ts": true, "-map_metadata": true, "-map_chapters": true,
}

var flagOptions = map[string]bool{
	"-noaccurate_seek": true, "-start_at_zero": true, "-copyts": true, "-y": true, "-nostats": true,
}

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrNotRemux, fmt.Sprintf(format, a...))
}

// Parse reads a Plex Transcoder argv. It accepts exactly a DASH stream of
// two outputs: output 0 a copied input stream, output 1 an input stream
// copied or converted to AAC (through Plex's stereo downmix or not).
func Parse(args []string, env map[string]string) (Job, error) {
	j := Job{Token: env["X_PLEX_TOKEN"]}
	if len(args) == 0 || args[len(args)-1] != "dash" {
		return j, refuse("the output is not dash")
	}
	args = args[:len(args)-1]
	var maps []string
	codecs := map[int]string{}
	input, filter := false, ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if flagOptions[a] {
			continue
		}
		if !valueOptions[a] || i+1 >= len(args) {
			return j, refuse("option %q", a)
		}
		v := args[i+1]
		i++
		switch a {
		case "-i":
			j.Input, input = v, true
		case "-ss":
			s, err := strconv.ParseFloat(v, 64)
			if err != nil || s < 0 {
				return j, refuse("-ss %q", v)
			}
			j.Start = time.Duration(s * float64(time.Second))
		case "-codec:0", "-codec:1":
			if input { // before -i they name Plex's decoders, which ffgo replaces
				codecs[int(a[len(a)-1]-'0')] = v
			}
		case "-map":
			maps = append(maps, v)
		case "-filter_complex":
			filter = v
		case "-metadata:s:1":
			if lang, ok := strings.CutPrefix(v, "language="); ok {
				j.AudioLanguage = lang
			}
		case "-b:1":
			n, err := bitRate(v)
			if err != nil {
				return j, refuse("-b:1 %q", v)
			}
			j.AudioBitRate = n
		case "-f":
			if v != "dash" {
				return j, refuse("format %q", v)
			}
		case "-dash_segment_type":
			if v != "mp4" {
				return j, refuse("segment type %q", v)
			}
		case "-init_seg_name":
			if v != InitName {
				return j, refuse("init name %q", v)
			}
		case "-media_seg_name":
			if v != MediaName {
				return j, refuse("media name %q", v)
			}
		case "-seg_duration":
			s, err := strconv.ParseFloat(v, 64)
			if err != nil || s <= 0 {
				return j, refuse("-seg_duration %q", v)
			}
			j.SegmentDuration = time.Duration(s * float64(time.Second))
		case "-skip_to_segment":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return j, refuse("-skip_to_segment %q", v)
			}
			j.SkipToSegment = n
		case "-progressurl":
			j.ProgressURL = v
		case "-manifest_name":
			j.ManifestURL = v
		}
	}
	if j.Input == "" || j.SegmentDuration == 0 || j.ManifestURL == "" || j.ProgressURL == "" {
		return j, refuse("missing input, segment duration, manifest or progress URL")
	}
	if j.SkipToSegment == 0 {
		j.SkipToSegment = 1
	}
	if len(maps) != 2 {
		return j, refuse("%d outputs, want 2", len(maps))
	}
	if codecs[0] != "copy" {
		return j, refuse("output 0 is %q, not a copy", codecs[0])
	}
	v := streamRef.FindStringSubmatch(maps[0])
	if v == nil {
		return j, refuse("output 0 maps %q", maps[0])
	}
	j.VideoStream, _ = strconv.Atoi(v[1])
	switch {
	case maps[1] == "[0]":
		m := downmix.FindStringSubmatch(filter)
		if m == nil || codecs[1] != "aac" {
			return j, refuse("audio filter %q with codec %q", filter, codecs[1])
		}
		j.AudioStream, _ = strconv.Atoi(m[1])
		j.AudioSampleRate, _ = strconv.Atoi(m[2])
		j.AudioChannels = 2
	case streamRef.MatchString(maps[1]) && filter == "":
		j.AudioStream, _ = strconv.Atoi(streamRef.FindStringSubmatch(maps[1])[1])
		switch codecs[1] {
		case "copy":
			j.AudioCopy, j.AudioBitRate = true, 0
		case "aac":
		default:
			return j, refuse("audio codec %q", codecs[1])
		}
	default:
		return j, refuse("output 1 maps %q with filter %q", maps[1], filter)
	}
	if !j.AudioCopy && j.AudioBitRate == 0 {
		return j, refuse("an AAC output with no bit rate")
	}
	return j, nil
}

// bitRate reads ffmpeg's "256k", "1M" or a plain number.
func bitRate(v string) (int64, error) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(v, "k"):
		mult, v = 1000, strings.TrimSuffix(v, "k")
	case strings.HasSuffix(v, "M"):
		mult, v = 1000000, strings.TrimSuffix(v, "M")
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n * mult, err
}
```

Note the parse tracks output codecs after `-i` only: `-codec:0`/`-codec:1` before `-i` are Plex's decoder choices (`hevc`, `eac3_eae`) and are ignored. Fix the switch accordingly: store into `codecs` only when `input` is true (as written), and do nothing before.

- [ ] **Step 6: Run the tests**

Run: `go test ./pkg/remux/`
Expected: PASS. If `TestEveryLoggedBrowserJobParses` fails on a job, print the refusal and look at that fixture line: extend the parser only for an option the 44 real jobs show, never by loosening the allow-list.

- [ ] **Step 7: Commit**

```bash
git add pkg/remux/testdata/plex-dash-jobs.log pkg/remux/testdata/plex-static.mpd
git commit -m "feat(remux): parse Plex Web's DASH jobs into a typed Job, refusing every option outside the 44 real jobs' shape" -- go.mod go.sum pkg/remux/job.go pkg/remux/parse.go pkg/remux/parse_test.go pkg/remux/words_test.go pkg/remux/testdata/plex-dash-jobs.log pkg/remux/testdata/plex-static.mpd
```

---

### Task 3: `pkg/remux` — rendering the manifest

**Files:**
- Create: `pkg/remux/mpd.go`, `pkg/remux/mpd_test.go`

**Interfaces:**
- Produces:
```go
type TimelineEntry struct{ T, D int64 } // in the representation's timescale
type Representation struct {
	Codecs    string // "hev1", "avc1", "mp4a.40.2"
	Bandwidth int64
	Width, Height int    // video
	FrameRate string     // video, e.g. "24/1"
	SampleRate, Channels int // audio
	Language  string     // audio
	Timescale int32
	Timeline  []TimelineEntry
}
type Manifest struct {
	Final           bool
	Duration        time.Duration // Final only
	StartNumber     int
	SegmentDuration time.Duration
	Start           time.Time     // the job's start, for a dynamic MPD
	Now             time.Time
	Video, Audio    Representation
}
func (m Manifest) Render() []byte
```

- [ ] **Step 1: Write the failing test** — `pkg/remux/mpd_test.go`:

```go
package remux

import (
	"encoding/xml"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mpdView struct {
	Type    string `xml:"type,attr"`
	Profile string `xml:"profiles,attr"`
	Sets    []struct {
		ContentType string `xml:"contentType,attr"`
		Reps        []struct {
			ID       string `xml:"id,attr"`
			Mime     string `xml:"mimeType,attr"`
			Codecs   string `xml:"codecs,attr"`
			Channels struct {
				Value string `xml:"value,attr"`
			} `xml:"AudioChannelConfiguration"`
			Template struct {
				Timescale string `xml:"timescale,attr"`
				Init      string `xml:"initialization,attr"`
				Media     string `xml:"media,attr"`
				Start     string `xml:"startNumber,attr"`
				S         []struct {
					T string `xml:"t,attr"`
					D string `xml:"d,attr"`
				} `xml:"SegmentTimeline>S"`
			} `xml:"SegmentTemplate"`
		} `xml:"Representation"`
	} `xml:"Period>AdaptationSet"`
}

func view(t *testing.T, b []byte) mpdView {
	t.Helper()
	var v mpdView
	require.NoError(t, xml.Unmarshal(b, &v))
	return v
}

// The final manifest Plex's own binary posted for a 30 s clip (recorded
// 2026-10-01) and ours for the same segments agree on everything a player
// reads.
func TestAFinalManifestHasPlexsShape(t *testing.T) {
	plex, err := os.ReadFile("testdata/plex-static.mpd")
	require.NoError(t, err)
	ours := Manifest{
		Final: true, Duration: 29900 * time.Millisecond, StartNumber: 1, SegmentDuration: 5 * time.Second,
		Video: Representation{Codecs: "hev1", Bandwidth: 485283, Width: 640, Height: 360, FrameRate: "24/1", Timescale: 12288,
			Timeline: []TimelineEntry{{258, 127476}, {127734, 128004}, {255738, 113148}}},
		Audio: Representation{Codecs: "mp4a.40.2", Bandwidth: 256000, SampleRate: 96000, Channels: 2, Language: "eng", Timescale: 96000,
			Timeline: []TimelineEntry{{0, 990208}, {990208, 1000448}, {1990656, 892928}}},
	}.Render()
	assert.Equal(t, view(t, plex), view(t, ours))
}

func TestARunningManifestIsDynamic(t *testing.T) {
	start := time.Date(2026, 10, 1, 16, 44, 21, 0, time.UTC)
	b := Manifest{StartNumber: 3, SegmentDuration: 5 * time.Second, Start: start, Now: start.Add(time.Second),
		Video: Representation{Codecs: "hev1", Timescale: 12288, Timeline: []TimelineEntry{{0, 61440}}},
		Audio: Representation{Codecs: "mp4a.40.2", Channels: 2, Timescale: 48000, Timeline: []TimelineEntry{{0, 240000}}},
	}.Render()
	v := view(t, b)
	assert.Equal(t, "dynamic", v.Type)
	assert.Equal(t, "3", v.Sets[0].Reps[0].Template.Start)
	assert.Contains(t, string(b), `availabilityStartTime="2026-10-01T16:44:21.000Z"`)
	assert.NotContains(t, string(b), "mediaPresentationDuration")
}
```

Note: the fixture's timeline writes only the first `t` and later `d`s; `view` sees `t` empty for later entries in both, which is why `Render` must also write `t` only on the first entry and on any entry that does not follow its predecessor.

- [ ] **Step 2: Run to verify it fails**

Run: `go test -run 'Manifest' ./pkg/remux/`
Expected: FAIL to build: `undefined: Manifest`.

- [ ] **Step 3: Implement** — `pkg/remux/mpd.go`:

```go
package remux

import (
	"bytes"
	"fmt"
	"time"
)

type TimelineEntry struct{ T, D int64 }

type Representation struct {
	Codecs               string
	Bandwidth            int64
	Width, Height        int
	FrameRate            string
	SampleRate, Channels int
	Language             string
	Timescale            int32
	Timeline             []TimelineEntry
}

// Manifest is the MPD the worker publishes after each segment: the shape
// Plex's own binary posts (ffmpeg's dashenc), dynamic while the job runs
// and static with the whole duration at its end.
type Manifest struct {
	Final           bool
	Duration        time.Duration
	StartNumber     int
	SegmentDuration time.Duration
	Start, Now      time.Time
	Video, Audio    Representation
}

func seconds(d time.Duration) string { return fmt.Sprintf("PT%.1fS", d.Seconds()) }

func (r Representation) longest() time.Duration {
	var max int64
	for _, e := range r.Timeline {
		max = Max(max, e.D)
	}
	if r.Timescale == 0 {
		return 0
	}
	return time.Duration(max) * time.Second / time.Duration(r.Timescale)
}

// Max is the larger of a and b.
func Max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (m Manifest) Render() []byte {
	var b bytes.Buffer
	longest := time.Duration(Max(int64(m.Video.longest()), int64(m.Audio.longest())))
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<MPD xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"` + "\n")
	b.WriteString("\txmlns=\"urn:mpeg:dash:schema:mpd:2011\"\n\txmlns:xlink=\"http://www.w3.org/1999/xlink\"\n")
	b.WriteString("\txsi:schemaLocation=\"urn:mpeg:DASH:schema:MPD:2011 http://standards.iso.org/ittf/PubliclyAvailableStandards/MPEG-DASH_schema_files/DASH-MPD.xsd\"\n")
	b.WriteString("\tprofiles=\"urn:mpeg:dash:profile:isoff-live:2011\"\n")
	if m.Final {
		fmt.Fprintf(&b, "\ttype=\"static\"\n\tmediaPresentationDuration=%q\n", seconds(m.Duration))
	} else {
		fmt.Fprintf(&b, "\ttype=\"dynamic\"\n\tminimumUpdatePeriod=%q\n\tsuggestedPresentationDelay=%q\n", seconds(2*m.SegmentDuration), seconds(2*m.SegmentDuration))
		fmt.Fprintf(&b, "\tavailabilityStartTime=%q\n\tpublishTime=%q\n", m.Start.UTC().Format("2006-01-02T15:04:05.000Z"), m.Now.UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	fmt.Fprintf(&b, "\tmaxSegmentDuration=%q\n\tminBufferTime=%q>\n", seconds(m.SegmentDuration), seconds(2*longest))
	b.WriteString("\t<ProgramInformation>\n\t</ProgramInformation>\n\t<ServiceDescription id=\"0\">\n\t</ServiceDescription>\n")
	b.WriteString("\t<Period id=\"0\" start=\"PT0.0S\">\n")
	v := m.Video
	fmt.Fprintf(&b, "\t\t<AdaptationSet id=\"0\" contentType=\"video\" startWithSAP=\"1\" segmentAlignment=\"true\" bitstreamSwitching=\"true\" frameRate=%q maxWidth=\"%d\" maxHeight=\"%d\" par=\"16:9\">\n", v.FrameRate, v.Width, v.Height)
	fmt.Fprintf(&b, "\t\t\t<Representation id=\"0\" mimeType=\"video/mp4\" codecs=%q bandwidth=\"%d\" width=\"%d\" height=\"%d\" sar=\"1:1\">\n", v.Codecs, v.Bandwidth, v.Width, v.Height)
	m.template(&b, v)
	b.WriteString("\t\t\t</Representation>\n\t\t</AdaptationSet>\n")
	a := m.Audio
	fmt.Fprintf(&b, "\t\t<AdaptationSet id=\"1\" contentType=\"audio\" startWithSAP=\"1\" segmentAlignment=\"true\" bitstreamSwitching=\"true\" lang=%q>\n", a.Language)
	fmt.Fprintf(&b, "\t\t\t<Representation id=\"1\" mimeType=\"audio/mp4\" codecs=%q bandwidth=\"%d\" audioSamplingRate=\"%d\">\n", a.Codecs, a.Bandwidth, a.SampleRate)
	fmt.Fprintf(&b, "\t\t\t\t<AudioChannelConfiguration schemeIdUri=\"urn:mpeg:dash:23003:3:audio_channel_configuration:2011\" value=\"%d\" />\n", a.Channels)
	m.template(&b, a)
	b.WriteString("\t\t\t</Representation>\n\t\t</AdaptationSet>\n\t</Period>\n</MPD>\n")
	return b.Bytes()
}

func (m Manifest) template(b *bytes.Buffer, r Representation) {
	fmt.Fprintf(b, "\t\t\t\t<SegmentTemplate timescale=\"%d\" initialization=%q media=%q startNumber=\"%d\">\n\t\t\t\t\t<SegmentTimeline>\n",
		r.Timescale, InitName, MediaName, m.StartNumber)
	next := int64(-1)
	for _, e := range r.Timeline {
		if e.T != next {
			fmt.Fprintf(b, "\t\t\t\t\t\t<S t=\"%d\" d=\"%d\" />\n", e.T, e.D)
		} else {
			fmt.Fprintf(b, "\t\t\t\t\t\t<S d=\"%d\" />\n", e.D)
		}
		next = e.T + e.D
	}
	b.WriteString("\t\t\t\t\t</SegmentTimeline>\n\t\t\t\t</SegmentTemplate>\n")
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -run 'Manifest' ./pkg/remux/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git commit -m "feat(remux): render the DASH manifest in the shape Plex's binary posts, dynamic while running and static at the end" -- pkg/remux/mpd.go pkg/remux/mpd_test.go
```

---

### Task 4: `pkg/remux` — cutting fragmented MP4 into init and segments

**Files:**
- Create: `pkg/remux/fmp4.go`, `pkg/remux/fmp4_test.go`

**Interfaces:**
- Produces:
```go
type Splitter struct {
	OnInit     func(init []byte) error     // ftyp+moov, once
	OnFragment func(fragment []byte) error // [styp][sidx]moof+mdat
}
func (s *Splitter) Write(p []byte) (int, error) // io.Writer
```

- [ ] **Step 1: Write the failing test** — `pkg/remux/fmp4_test.go`:

```go
package remux

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func box(typ string, body string) []byte {
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// The writer muxer delivers bytes in AVIO-buffer chunks that cut boxes
// anywhere; the splitter returns whole init and whole fragments.
func TestTheSplitterCutsInitAndFragmentsAcrossWrites(t *testing.T) {
	init := cat(box("ftyp", "iso5"), box("moov", "tracks"))
	f1 := cat(box("moof", "one"), box("mdat", "AAAA"))
	f2 := cat(box("styp", "msdh"), box("moof", "two"), box("mdat", "BBBBBBBB"))
	stream := cat(init, f1, f2, box("mfra", "index"))

	var gotInit []byte
	var frags [][]byte
	s := &Splitter{
		OnInit:     func(b []byte) error { gotInit = append([]byte(nil), b...); return nil },
		OnFragment: func(b []byte) error { frags = append(frags, append([]byte(nil), b...)); return nil },
	}
	for i := 0; i < len(stream); i += 5 {
		n, err := s.Write(stream[i:min(i+5, len(stream))])
		require.NoError(t, err)
		require.Equal(t, min(5, len(stream)-i), n)
	}
	assert.Equal(t, init, gotInit)
	assert.Equal(t, [][]byte{f1, f2}, frags)
}

func TestAnUnsizedBoxIsAnError(t *testing.T) {
	b := box("mdat", "x")
	binary.BigEndian.PutUint32(b, 0) // "to the end of the file": no length to cut on
	_, err := (&Splitter{OnInit: func([]byte) error { return nil }, OnFragment: func([]byte) error { return nil }}).Write(b)
	assert.Error(t, err)
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -run Splitter ./pkg/remux/`
Expected: FAIL to build: `undefined: Splitter`.

- [ ] **Step 3: Implement** — `pkg/remux/fmp4.go`:

```go
package remux

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Splitter is the io.Writer a fragmented-MP4 muxer writes into. It hands
// the init (ftyp+moov) to OnInit once, and each fragment -- any styp/sidx,
// then moof and its mdat -- to OnFragment. Boxes after the last fragment
// (mfra) are dropped.
type Splitter struct {
	OnInit     func(init []byte) error
	OnFragment func(fragment []byte) error

	buf      []byte
	init     []byte
	frag     []byte
	initDone bool
	err      error
}

func (s *Splitter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	s.buf = append(s.buf, p...)
	for len(s.buf) >= 8 {
		size, hdr := uint64(binary.BigEndian.Uint32(s.buf)), uint64(8)
		if size == 1 {
			if len(s.buf) < 16 {
				break
			}
			size, hdr = binary.BigEndian.Uint64(s.buf[8:]), 16
		}
		if size == 0 {
			s.err = errors.New("fmp4: a box sized to the end of the file")
			return 0, s.err
		}
		if size < hdr {
			s.err = fmt.Errorf("fmp4: box size %d", size)
			return 0, s.err
		}
		if uint64(len(s.buf)) < size {
			break
		}
		b := s.buf[:size]
		if err := s.box(string(b[4:8]), b); err != nil {
			s.err = err
			return 0, err
		}
		s.buf = s.buf[size:]
	}
	return len(p), nil
}

func (s *Splitter) box(typ string, b []byte) error {
	switch typ {
	case "ftyp":
		s.init = append(s.init, b...)
	case "moov":
		s.init = append(s.init, b...)
		s.initDone = true
		return s.OnInit(s.init)
	case "styp", "sidx", "moof":
		s.frag = append(s.frag, b...)
	case "mdat":
		if !s.initDone {
			return errors.New("fmp4: media before the init")
		}
		f := append(s.frag, b...)
		s.frag = nil
		return s.OnFragment(f)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -run 'Splitter|Unsized' ./pkg/remux/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git commit -m "feat(remux): cut a fragmented MP4 stream into its init and whole fragments" -- pkg/remux/fmp4.go pkg/remux/fmp4_test.go
```

---

### Task 5: `pkg/remux/cache` — the remux cache

**Files:**
- Create: `pkg/remux/segment.go` (shared types)
- Create: `pkg/remux/cache/cache.go`, `pkg/remux/cache/cache_test.go`

**Interfaces:**
- Produces (`pkg/remux/segment.go`):
```go
type Fragment struct {
	Data []byte
	T, D int64 // start and duration in the representation's timescale
}
type Segment struct {
	N            int
	Start, End   time.Duration // source time; the next segment of a chain starts at End
	Video, Audio Fragment
}
// StreamInfo is what the manifest says of the streams; the cache keeps it
// so a replay can publish a manifest without opening the file.
type StreamInfo struct {
	VideoCodec, AudioCodec string // MPD codecs: "hev1"/"avc1", "mp4a.40.2"
	Width, Height          int
	FrameRate              string
	SampleRate, Channels   int
	Duration               time.Duration
}
```
- Produces (`pkg/remux/cache`):
```go
type Key struct {
	Input           string
	Size            int64
	ModTime         time.Time
	AudioStream     int
	AudioCopy       bool
	Channels        int
	SampleRate      int
	BitRate         int64
	SegmentDuration time.Duration
}
func (k Key) String() string
type Cache struct { Dir string; MaxBytes int64; Now func() time.Time }
func (c *Cache) Open(k Key) (*Entry, error)
func (c *Cache) Evict() error
func (e *Entry) Init() (video, audio []byte, timescales [2]int32, info remux.StreamInfo, ok bool)
func (e *Entry) SetInit(video, audio []byte, timescales [2]int32, info remux.StreamInfo) error
func (e *Entry) Segment(n int) (remux.Segment, bool, error)
func (e *Entry) Put(s remux.Segment) error
func (e *Entry) Last() int
func (e *Entry) SetLast(n int) error
```

- [ ] **Step 1: Write the shared types** — `pkg/remux/segment.go`:

```go
package remux

import "time"

// Fragment is one representation's part of a segment: a moof+mdat.
type Fragment struct {
	Data []byte
	T, D int64
}

// Segment is segment N of a stream. Start and End are source time, so a
// cached segment continues a chain only from a segment that ended where it
// starts.
type Segment struct {
	N            int
	Start, End   time.Duration
	Video, Audio Fragment
}

// StreamInfo is what the manifest says of the streams; the cache keeps it
// so a replay can publish a manifest without opening the file.
type StreamInfo struct {
	VideoCodec, AudioCodec string
	Width, Height          int
	FrameRate              string
	SampleRate, Channels   int
	Duration               time.Duration
}
```

- [ ] **Step 2: Write the failing tests** — `pkg/remux/cache/cache_test.go`:

```go
package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/remux"
)

func key(input string) Key {
	return Key{Input: input, Size: 10, ModTime: time.Unix(100, 0), AudioStream: 1, Channels: 2,
		SampleRate: 96000, BitRate: 256000, SegmentDuration: 5 * time.Second}
}

func seg(n int, start time.Duration) remux.Segment {
	return remux.Segment{N: n, Start: start, End: start + 5*time.Second,
		Video: remux.Fragment{Data: []byte{byte(n), 'v'}, T: int64(n), D: 1},
		Audio: remux.Fragment{Data: []byte{byte(n), 'a'}, T: int64(n), D: 1}}
}

func TestSegmentsComeBackByNumberWhereverTheyLie(t *testing.T) {
	c := &Cache{Dir: t.TempDir()}
	e, err := c.Open(key("/m/a.mkv"))
	require.NoError(t, err)
	info := remux.StreamInfo{VideoCodec: "hev1", Width: 640, Height: 360, Channels: 2, Duration: time.Minute}
	require.NoError(t, e.SetInit([]byte("vinit"), []byte("ainit"), [2]int32{12288, 96000}, info))
	require.NoError(t, e.Put(seg(5, 20*time.Second))) // a seek run first
	require.NoError(t, e.Put(seg(1, 0)))
	require.NoError(t, e.Put(seg(1, 0)), "a segment already held is not appended twice")

	e2, err := c.Open(key("/m/a.mkv")) // a later job reads the index from disk
	require.NoError(t, err)
	v, a, ts, gotInfo, ok := e2.Init()
	require.True(t, ok)
	assert.Equal(t, info, gotInfo, "a replay publishes its manifest from this")
	assert.Equal(t, "vinit", string(v))
	assert.Equal(t, "ainit", string(a))
	assert.Equal(t, [2]int32{12288, 96000}, ts)
	got, ok, err := e2.Segment(5)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, seg(5, 20*time.Second), got)
	_, ok, _ = e2.Segment(2)
	assert.False(t, ok)
	fi, err := os.Stat(filepath.Join(c.Dir, key("/m/a.mkv").String(), "video.mp4"))
	require.NoError(t, err)
	assert.Equal(t, int64(len("vinit")+2+2), fi.Size(), "init once, then each fragment once")
}

func TestADifferentInitResetsTheEntry(t *testing.T) {
	c := &Cache{Dir: t.TempDir()}
	e, _ := c.Open(key("/m/a.mkv"))
	require.NoError(t, e.SetInit([]byte("old"), []byte("old"), [2]int32{1, 1}, remux.StreamInfo{}))
	require.NoError(t, e.Put(seg(1, 0)))
	require.NoError(t, e.SetInit([]byte("new"), []byte("new"), [2]int32{1, 1}, remux.StreamInfo{}))
	_, ok, _ := e.Segment(1)
	assert.False(t, ok, "segments muxed against another init are dropped")
	v, _, _, _, _ := e.Init()
	assert.Equal(t, "new", string(v))
}

func TestAChangedFileIsAnotherEntry(t *testing.T) {
	k := key("/m/a.mkv")
	k2 := k
	k2.ModTime = k.ModTime.Add(time.Second)
	assert.NotEqual(t, k.String(), k2.String())
}

func TestEvictionRemovesTheLeastRecentlyUsed(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &Cache{Dir: t.TempDir(), MaxBytes: 30, Now: func() time.Time { return now }}
	for i, name := range []string{"/m/old.mkv", "/m/new.mkv"} {
		now = now.Add(time.Duration(i) * time.Hour)
		e, err := c.Open(key(name))
		require.NoError(t, err)
		require.NoError(t, e.SetInit(make([]byte, 10), make([]byte, 10), [2]int32{1, 1}, remux.StreamInfo{}))
	}
	require.NoError(t, c.Evict())
	_, err := os.Stat(filepath.Join(c.Dir, key("/m/old.mkv").String()))
	assert.True(t, os.IsNotExist(err), "the older entry goes")
	_, err = os.Stat(filepath.Join(c.Dir, key("/m/new.mkv").String()))
	assert.NoError(t, err)
}
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./pkg/remux/cache/`
Expected: FAIL to build: `undefined: Cache`.

- [ ] **Step 4: Implement** — `pkg/remux/cache/cache.go`:

```go
// Package cache keeps the segments the remux worker produced, per file and
// audio choice, on the worker's own disk: video.mp4 and audio.mp4 are
// fragmented MP4s (the init, then fragments in the order produced) and
// index.json finds each segment by number.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/mediactl/clusterplex/pkg/remux"
)

type Key struct {
	Input           string
	Size            int64
	ModTime         time.Time
	AudioStream     int
	AudioCopy       bool
	Channels        int
	SampleRate      int
	BitRate         int64
	SegmentDuration time.Duration
}

func (k Key) String() string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%d|%t|%d|%d|%d|%d", k.Input, k.Size, k.ModTime.UnixNano(),
		k.AudioStream, k.AudioCopy, k.Channels, k.SampleRate, k.BitRate, k.SegmentDuration)))
	return hex.EncodeToString(h[:16])
}

type span struct{ Off, Len int64 }

type part struct {
	span
	T, D int64
}

type record struct {
	Start, End   time.Duration
	Video, Audio part
}

type index struct {
	VideoInit, AudioInit span
	Timescales           [2]int32
	Info                 remux.StreamInfo
	HasInit              bool
	Segments             map[string]record
	Last                 int
	Used                 time.Time
}

type Cache struct {
	Dir      string
	MaxBytes int64
	Now      func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

type Entry struct {
	c   *Cache
	dir string
	mu  *sync.Mutex
	idx index
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Open returns k's entry, creating its directory; two jobs for one key on
// one worker share its lock.
func (c *Cache) Open(k Key) (*Entry, error) {
	c.mu.Lock()
	if c.locks == nil {
		c.locks = map[string]*sync.Mutex{}
	}
	name := k.String()
	l, ok := c.locks[name]
	if !ok {
		l = &sync.Mutex{}
		c.locks[name] = l
	}
	c.mu.Unlock()
	e := &Entry{c: c, dir: filepath.Join(c.Dir, name), mu: l}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := os.MkdirAll(e.dir, 0o755); err != nil {
		return nil, err
	}
	if err := e.load(); err != nil {
		return nil, err
	}
	e.idx.Used = c.now()
	return e, e.save()
}

func (e *Entry) load() error {
	b, err := os.ReadFile(filepath.Join(e.dir, "index.json"))
	if os.IsNotExist(err) {
		e.idx = index{Segments: map[string]record{}}
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &e.idx); err != nil {
		return e.resetLocked()
	}
	if e.idx.Segments == nil {
		e.idx.Segments = map[string]record{}
	}
	return nil
}

// save writes the index by rename, after the bytes it points at are synced.
func (e *Entry) save() error {
	b, err := json.Marshal(e.idx)
	if err != nil {
		return err
	}
	tmp := filepath.Join(e.dir, "index.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(e.dir, "index.json"))
}

func (e *Entry) resetLocked() error {
	for _, f := range []string{"video.mp4", "audio.mp4", "index.json"} {
		if err := os.Remove(filepath.Join(e.dir, f)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	e.idx = index{Segments: map[string]record{}, Used: e.c.now()}
	return nil
}

func appendTo(path string, data []byte) (span, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return span{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return span{}, err
	}
	if _, err := f.Write(data); err != nil {
		return span{}, err
	}
	return span{Off: fi.Size(), Len: int64(len(data))}, f.Sync()
}

func readAt(path string, s span) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, s.Len)
	_, err = io.ReadFull(io.NewSectionReader(f, s.Off, s.Len), b)
	return b, err
}

func (e *Entry) Init() (video, audio []byte, timescales [2]int32, info remux.StreamInfo, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.idx.HasInit {
		return nil, nil, timescales, info, false
	}
	v, err1 := readAt(filepath.Join(e.dir, "video.mp4"), e.idx.VideoInit)
	a, err2 := readAt(filepath.Join(e.dir, "audio.mp4"), e.idx.AudioInit)
	if err1 != nil || err2 != nil {
		return nil, nil, timescales, info, false
	}
	return v, a, e.idx.Timescales, e.idx.Info, true
}

// SetInit records the inits; an entry holding different ones is reset, as
// its fragments were muxed against them.
func (e *Entry) SetInit(video, audio []byte, timescales [2]int32, info remux.StreamInfo) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.idx.HasInit {
		v, err1 := readAt(filepath.Join(e.dir, "video.mp4"), e.idx.VideoInit)
		a, err2 := readAt(filepath.Join(e.dir, "audio.mp4"), e.idx.AudioInit)
		if err1 == nil && err2 == nil && slices.Equal(v, video) && slices.Equal(a, audio) && e.idx.Timescales == timescales {
			e.idx.Info = info
			return e.save()
		}
		if err := e.resetLocked(); err != nil {
			return err
		}
	}
	vs, err := appendTo(filepath.Join(e.dir, "video.mp4"), video)
	if err != nil {
		return err
	}
	as, err := appendTo(filepath.Join(e.dir, "audio.mp4"), audio)
	if err != nil {
		return err
	}
	e.idx.VideoInit, e.idx.AudioInit, e.idx.Timescales, e.idx.Info, e.idx.HasInit = vs, as, timescales, info, true
	return e.save()
}

func (e *Entry) Segment(n int) (remux.Segment, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.idx.Segments[strconv.Itoa(n)]
	if !ok {
		return remux.Segment{}, false, nil
	}
	v, err := readAt(filepath.Join(e.dir, "video.mp4"), r.Video.span)
	if err != nil {
		return remux.Segment{}, false, err
	}
	a, err := readAt(filepath.Join(e.dir, "audio.mp4"), r.Audio.span)
	if err != nil {
		return remux.Segment{}, false, err
	}
	e.idx.Used = e.c.now()
	return remux.Segment{N: n, Start: r.Start, End: r.End,
		Video: remux.Fragment{Data: v, T: r.Video.T, D: r.Video.D},
		Audio: remux.Fragment{Data: a, T: r.Audio.T, D: r.Audio.D}}, true, nil
}

func (e *Entry) Put(s remux.Segment) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.idx.HasInit {
		return fmt.Errorf("cache: segment %d before the init", s.N)
	}
	if _, ok := e.idx.Segments[strconv.Itoa(s.N)]; ok {
		return nil
	}
	vs, err := appendTo(filepath.Join(e.dir, "video.mp4"), s.Video.Data)
	if err != nil {
		return err
	}
	as, err := appendTo(filepath.Join(e.dir, "audio.mp4"), s.Audio.Data)
	if err != nil {
		return err
	}
	e.idx.Segments[strconv.Itoa(s.N)] = record{Start: s.Start, End: s.End,
		Video: part{span: vs, T: s.Video.T, D: s.Video.D}, Audio: part{span: as, T: s.Audio.T, D: s.Audio.D}}
	e.idx.Used = e.c.now()
	return e.save()
}

// Last is the number of the file's final segment once a run reached its
// end, 0 before.
func (e *Entry) Last() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.idx.Last
}

func (e *Entry) SetLast(n int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.idx.Last = n
	return e.save()
}

// Evict removes the least recently used entries until the cache holds at
// most MaxBytes; 0 keeps everything.
func (c *Cache) Evict() error {
	if c.MaxBytes <= 0 {
		return nil
	}
	ents, err := os.ReadDir(c.Dir)
	if err != nil {
		return err
	}
	type entry struct {
		name string
		size int64
		used time.Time
	}
	var all []entry
	var total int64
	for _, d := range ents {
		if !d.IsDir() {
			continue
		}
		var size int64
		for _, f := range []string{"video.mp4", "audio.mp4"} {
			if fi, err := os.Stat(filepath.Join(c.Dir, d.Name(), f)); err == nil {
				size += fi.Size()
			}
		}
		var idx index
		if b, err := os.ReadFile(filepath.Join(c.Dir, d.Name(), "index.json")); err == nil {
			_ = json.Unmarshal(b, &idx)
		}
		all = append(all, entry{d.Name(), size, idx.Used})
		total += size
	}
	slices.SortFunc(all, func(a, b entry) int { return a.used.Compare(b.used) })
	for _, e := range all {
		if total <= c.MaxBytes {
			break
		}
		if err := os.RemoveAll(filepath.Join(c.Dir, e.name)); err != nil {
			return err
		}
		total -= e.size
	}
	return nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./pkg/remux/cache/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git commit -m "feat(remux): a per-worker remux cache -- fragmented MP4 per representation, an index by segment number, reset on a different init, LRU eviction" -- pkg/remux/segment.go pkg/remux/cache/cache.go pkg/remux/cache/cache_test.go
```

---

### Task 6: who is playing — `/status/sessions` and `Classify`

**Files:**
- Modify: `pkg/plex/api/client.go`, `pkg/plex/api/client_test.go`
- Create: `pkg/remux/classify.go`, `pkg/remux/classify_test.go`

**Interfaces:**
- Produces: `func (c *plexapi.Client) PlayerProduct(ctx context.Context, transcodeSession string) (string, error)`;
  `type Players interface{ PlayerProduct(ctx context.Context, transcodeSession string) (string, error) }`;
  `func Classify(ctx context.Context, req *pb.ExecRequest, players Players) (Job, bool)`;
  `const PlexWeb = "Plex Web"`.

- [ ] **Step 1: Write the failing tests**

Append to `pkg/plex/api/client_test.go`:
```go
// /status/sessions as PMS answers it: each playback names its player and,
// while transcoding, its transcode session's key.
func TestItFindsThePlayerOfATranscodeSession(t *testing.T) {
	c, _ := recorder(t, map[string]string{
		"GET /status/sessions": `{"MediaContainer":{"Metadata":[
			{"Player":{"product":"Plex for Android (TV)"},"TranscodeSession":{"key":"/transcode/sessions/other"}},
			{"Player":{"product":"Plex Web","platform":"Firefox"},"TranscodeSession":{"key":"/transcode/sessions/f168tc7vl2desp78pov5ktwu"}}]}}`,
	}, nil)
	got, err := c.PlayerProduct(t.Context(), "f168tc7vl2desp78pov5ktwu")
	require.NoError(t, err)
	assert.Equal(t, "Plex Web", got)
	got, err = c.PlayerProduct(t.Context(), "missing")
	require.NoError(t, err)
	assert.Empty(t, got)
}
```

`pkg/remux/classify_test.go`:
```go
package remux

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	pb "github.com/mediactl/clusterplex/proto"
)

type players map[string]string

func (p players) PlayerProduct(_ context.Context, id string) (string, error) {
	if v, ok := p[id]; ok {
		return v, nil
	}
	return "", errors.New("unreachable")
}

type slow struct{}

func (slow) PlayerProduct(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func arcaneRequest() *pb.ExecRequest {
	args, env := loggedJob(arcane)
	return &pb.ExecRequest{TargetBinary: "Plex Transcoder", Args: args, Env: env}
}

func TestOnlyPlexWebsRemuxIsRouted(t *testing.T) {
	ctx := t.Context()
	_, ok := Classify(ctx, arcaneRequest(), players{"f168tc7vl2desp78pov5ktwu": "Plex Web"})
	assert.True(t, ok)
	_, ok = Classify(ctx, arcaneRequest(), players{"f168tc7vl2desp78pov5ktwu": "Plex for Android (TV)"})
	assert.False(t, ok, "another client's DASH job stays Plex's")
	_, ok = Classify(ctx, arcaneRequest(), players{})
	assert.False(t, ok, "a session Plex cannot name stays Plex's")
	req := arcaneRequest()
	req.TargetBinary = "Plex Media Scanner"
	_, ok = Classify(ctx, req, players{"f168tc7vl2desp78pov5ktwu": "Plex Web"})
	assert.False(t, ok)
}

func TestASlowSessionLookupFallsBackToPlex(t *testing.T) {
	start := time.Now()
	_, ok := Classify(t.Context(), arcaneRequest(), slow{})
	assert.False(t, ok)
	assert.Less(t, time.Since(start), 3*time.Second)
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./pkg/plex/api/ ./pkg/remux/`
Expected: FAIL to build: `c.PlayerProduct undefined`, `undefined: Classify`.

- [ ] **Step 3: Implement**

In `pkg/plex/api/client.go` after `SetSectionPrefs`:
```go
// PlayerProduct is the product of the player whose playback is transcode
// session id (e.g. "Plex Web"), from /status/sessions; "" when no playback
// names that session.
func (c *Client) PlayerProduct(ctx context.Context, id string) (string, error) {
	var out struct {
		MediaContainer struct {
			Metadata []struct {
				Player struct {
					Product string `json:"product"`
				} `json:"Player"`
				TranscodeSession struct {
					Key string `json:"key"`
				} `json:"TranscodeSession"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := c.do(ctx, http.MethodGet, "/status/sessions", nil, &out); err != nil {
		return "", err
	}
	for _, m := range out.MediaContainer.Metadata {
		if k := m.TranscodeSession.Key; k == id || k == "/transcode/sessions/"+id {
			return m.Player.Product, nil
		}
	}
	return "", nil
}
```

`pkg/remux/classify.go`:
```go
package remux

import (
	"context"
	"time"

	pb "github.com/mediactl/clusterplex/proto"
)

// PlexWeb is the product Plex's web player names itself.
const PlexWeb = "Plex Web"

// lookupTimeout bounds asking Plex who is playing; past it the job is Plex's.
const lookupTimeout = 2 * time.Second

type Players interface {
	PlayerProduct(ctx context.Context, transcodeSession string) (string, error)
}

// Classify reports whether req is a browser remux this pool runs: Plex's
// transcoder, a DASH job Parse accepts, for a session Plex says Plex Web
// is playing. Any doubt leaves it to Plex.
func Classify(ctx context.Context, req *pb.ExecRequest, players Players) (Job, bool) {
	if req.GetTargetBinary() != "Plex Transcoder" {
		return Job{}, false
	}
	j, err := Parse(req.GetArgs(), req.GetEnv())
	if err != nil {
		return Job{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	product, err := players.PlayerProduct(ctx, j.SessionID())
	if err != nil || product != PlexWeb {
		return Job{}, false
	}
	return j, true
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./pkg/plex/api/ ./pkg/remux/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git commit -m "feat(remux): classify a shimmed job as Plex Web's remux -- Plex's transcoder, a DASH job Parse accepts, and /status/sessions naming Plex Web within 2 s" -- pkg/plex/api/client.go pkg/plex/api/client_test.go pkg/remux/classify.go pkg/remux/classify_test.go
```

---

### Task 7: the Remux gRPC contract

**Files:**
- Create: `proto/remux.proto`, generated `proto/remuxpb/remux.pb.go`, `proto/remuxpb/remux_grpc.pb.go`
- Modify: `Makefile` (the `proto` target)
- Create: `pkg/remux/wire.go`, `pkg/remux/wire_test.go`

**Interfaces:**
- Produces: package `remuxpb` with `RemuxClient`, `RemuxServer`, `Job`, `Event` (oneof `File{name,data}`, `Manifest{mpd}`, `Progress{path,query}`, `Done{error}`); `func (j Job) Proto() *remuxpb.Job`; `func JobFromProto(p *remuxpb.Job) Job`. The token, progress and manifest URLs never leave the manager: they are not in `remuxpb.Job`.

- [ ] **Step 1: Write the proto** — `proto/remux.proto`:

```proto
syntax = "proto3";
package remux;
option go_package = "github.com/mediactl/clusterplex/proto/remuxpb";

// Remux runs one browser remux on a remux worker. The worker never talks to
// Plex: segments, manifests and progress all come back on the stream, and
// the serving pod's manager delivers them.
service Remux {
    rpc Remux(Job) returns (stream Event);
}

message Job {
    string input = 1;
    int64 start_ms = 2;
    int32 skip_to_segment = 3;
    int64 segment_ms = 4;
    int32 video_stream = 5;
    int32 audio_stream = 6;
    bool audio_copy = 7;
    int32 audio_channels = 8;
    int32 audio_sample_rate = 9;
    int64 audio_bit_rate = 10;
    string audio_language = 11;
}

message Event {
    oneof kind {
        File file = 1;
        Manifest manifest = 2;
        Progress progress = 3;
        Done done = 4;
    }
}

// File is a segment file to write into the session directory.
message File {
    string name = 1;
    bytes data = 2;
}

// Manifest is posted to Plex once the files it lists are written.
message Manifest {
    bytes mpd = 1;
}

// Progress is a PUT under the job's progress URL: path "" , "stream" or
// "streamDetail", query the already-encoded query string.
message Progress {
    string path = 1;
    string query = 2;
}

// Done ends the stream; an empty error is success.
message Done {
    string error = 1;
}
```

- [ ] **Step 2: Generate** — in `Makefile`, make the `proto` recipe also run:
```make
	mkdir -p proto/remuxpb
	protoc -I proto --go_out=proto/remuxpb --go_opt=paths=source_relative \
		--go-grpc_out=proto/remuxpb --go-grpc_opt=paths=source_relative proto/remux.proto
```
Run: `make proto && go build ./proto/...`
Expected: `proto/remuxpb/remux.pb.go` and `remux_grpc.pb.go` exist; the build succeeds.

- [ ] **Step 3: Write the failing test** — `pkg/remux/wire_test.go`:
```go
package remux

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAJobCrossesTheWireWithoutItsSecrets(t *testing.T) {
	args, env := loggedJob(arcane)
	j, err := Parse(args, env)
	assert.NoError(t, err)
	back := JobFromProto(j.Proto())
	assert.Equal(t, 495*time.Second, back.Start)
	assert.Equal(t, j.AudioSampleRate, back.AudioSampleRate)
	want := j
	want.Token, want.ProgressURL, want.ManifestURL = "", "", ""
	assert.Equal(t, want, back)
}
```
Run: `go test -run Wire ./pkg/remux/` — Expected: FAIL to build: `j.Proto undefined`.

- [ ] **Step 4: Implement** — `pkg/remux/wire.go`:
```go
package remux

import (
	"time"

	"github.com/mediactl/clusterplex/proto/remuxpb"
)

// Proto is the job as a worker receives it: no token, no Plex URL.
func (j Job) Proto() *remuxpb.Job {
	return &remuxpb.Job{
		Input: j.Input, StartMs: j.Start.Milliseconds(), SkipToSegment: int32(j.SkipToSegment),
		SegmentMs: j.SegmentDuration.Milliseconds(), VideoStream: int32(j.VideoStream),
		AudioStream: int32(j.AudioStream), AudioCopy: j.AudioCopy, AudioChannels: int32(j.AudioChannels),
		AudioSampleRate: int32(j.AudioSampleRate), AudioBitRate: j.AudioBitRate, AudioLanguage: j.AudioLanguage,
	}
}

func JobFromProto(p *remuxpb.Job) Job {
	return Job{
		Input: p.GetInput(), Start: time.Duration(p.GetStartMs()) * time.Millisecond,
		SkipToSegment: int(p.GetSkipToSegment()), SegmentDuration: time.Duration(p.GetSegmentMs()) * time.Millisecond,
		VideoStream: int(p.GetVideoStream()), AudioStream: int(p.GetAudioStream()), AudioCopy: p.GetAudioCopy(),
		AudioChannels: int(p.GetAudioChannels()), AudioSampleRate: int(p.GetAudioSampleRate()),
		AudioBitRate: p.GetAudioBitRate(), AudioLanguage: p.GetAudioLanguage(),
	}
}
```
Run: `go test ./pkg/remux/` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add proto/remux.proto proto/remuxpb
git commit -m "feat(remux): the Remux gRPC contract -- a job without Plex's token or URLs in, files, manifests, progress and done out" -- Makefile proto/remux.proto proto/remuxpb pkg/remux/wire.go pkg/remux/wire_test.go
```

---

### Task 8: `pkg/remux/pipeline` — the ffgo pipeline

**Files:**
- Create: `pkg/remux/pipeline/pipeline.go`, `pkg/remux/pipeline/boundaries.go`, `pkg/remux/pipeline/pipeline_test.go`
- Modify: `go.mod`, `go.sum` (`go mod tidy` now that ffgo is imported)

**Interfaces:**
- Consumes: `remux.Job`, `remux.Splitter`, `remux.Segment`, `remux.Fragment`; ffgo `NewMuxerToWriter`, `Muxer.Flush`, `MuxerStream.OutputTimeBase` (Task 1).
- Produces:
```go
type Sink interface {
	Init(video, audio []byte, timescales [2]int32, info remux.StreamInfo) error
	Segment(s remux.Segment) error
	Progress(path, query string)
}
type Options struct {
	From    int           // first segment number
	StartAt time.Duration // exact start of segment From when known (a cache chain); <0 applies the rule
}
func Run(ctx context.Context, job remux.Job, o Options, sink Sink) (last int, err error)
```

- [ ] **Step 1: Write the failing tests** — `pkg/remux/pipeline/pipeline_test.go`:

```go
package pipeline

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/remux"
)

// clip is an HEVC + AAC 5.1 MKV of seconds s with a keyframe every gop
// frames at 24 fps: the shape of the library's files.
func clip(t *testing.T, s, gop int) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg to make the clip")
	}
	if err := ffgo.Init(); err != nil {
		t.Skipf("FFmpeg libraries not available: %v", err)
	}
	out := filepath.Join(t.TempDir(), "clip.mkv")
	cmd := exec.Command("ffmpeg", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000,aformat=channel_layouts=5.1",
		"-t", strconv.Itoa(s), "-c:v", "libx265", "-preset", "ultrafast",
		"-x265-params", "log-level=none:keyint="+strconv.Itoa(gop)+":min-keyint="+strconv.Itoa(gop)+":scenecut=0",
		"-pix_fmt", "yuv420p10le", "-c:a", "aac", "-b:a", "384k", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make clip: %v %s", err, b)
	}
	return out
}

func job(input string, skip int) remux.Job {
	return remux.Job{Input: input, SkipToSegment: skip, Start: time.Duration(skip-1) * 5 * time.Second,
		SegmentDuration: 5 * time.Second, VideoStream: 0, AudioStream: 1,
		AudioChannels: 2, AudioSampleRate: 48000, AudioBitRate: 256000, AudioLanguage: "eng"}
}

type recorder struct {
	init     [2][]byte
	scales   [2]int32
	info     StreamInfo
	segments []remux.Segment
	progress []string
}

func (r *recorder) Init(v, a []byte, ts [2]int32, info remux.StreamInfo) error {
	r.init, r.scales, r.info = [2][]byte{v, a}, ts, info
	return nil
}
func (r *recorder) Segment(s remux.Segment) error { r.segments = append(r.segments, s); return nil }
func (r *recorder) Progress(path, query string)   { r.progress = append(r.progress, path+"?"+query) }

func run(t *testing.T, j remux.Job, o Options) *recorder {
	t.Helper()
	r := &recorder{}
	_, err := Run(context.Background(), j, o, r)
	require.NoError(t, err)
	return r
}

// write the init and the segments as one file per representation and count
// what ffprobe decodes from it.
func decodes(t *testing.T, init []byte, frags [][]byte) int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rep.mp4")
	require.NoError(t, os.WriteFile(path, append(init, bytes.Join(frags, nil)...), 0o644))
	out, err := exec.Command("ffprobe", "-v", "error", "-count_frames", "-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", path).Output()
	require.NoError(t, err)
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	return n
}

func TestAFullRunCutsDecodableSegmentsAtKeyframes(t *testing.T) {
	r := run(t, job(clip(t, 20, 48), 1), Options{From: 1, StartAt: -1}) // a keyframe every 2 s
	// keyframes every 2 s: the first at or after 0, 5, 10 and 15 s
	want := []time.Duration{0, 6 * time.Second, 10 * time.Second, 16 * time.Second}
	require.Len(t, r.segments, len(want))
	var v [][]byte
	for i, s := range r.segments {
		assert.Equal(t, i+1, s.N)
		assert.Equal(t, want[i], s.Start, "segment %d", s.N)
		if i > 0 {
			assert.Equal(t, r.segments[i-1].End, s.Start)
		}
		v = append(v, s.Video.Data)
	}
	assert.Equal(t, 480, decodes(t, r.init[0], v), "every video frame, once")
	assert.Equal(t, "hev1", r.info.VideoCodec)
	assert.Equal(t, 2, r.info.Channels)
}

// Review focus 1: a keyframe interval (10 s) longer than a segment (5 s).
func TestLongGOPSegmentsAreContiguous(t *testing.T) {
	r := run(t, job(clip(t, 30, 240), 1), Options{From: 1, StartAt: -1})
	var starts []time.Duration
	for i, s := range r.segments {
		assert.Equal(t, i+1, s.N, "numbers stay contiguous")
		starts = append(starts, s.Start)
	}
	assert.Equal(t, []time.Duration{0, 10 * time.Second, 20 * time.Second}, starts)
	// a seek to segment 3 (10 s) starts at the boundary the rule gives
	seek := run(t, job(clip(t, 30, 240), 3), Options{From: 3, StartAt: -1})
	assert.Equal(t, 10*time.Second, seek.segments[0].Start)
}

func TestASeekRunsVideoIsTheFullRunsVideo(t *testing.T) {
	in := clip(t, 20, 48)
	full := run(t, job(in, 1), Options{From: 1, StartAt: -1})
	seek := run(t, job(in, 3), Options{From: 3, StartAt: -1})
	require.Equal(t, 3, seek.segments[0].N)
	assert.Equal(t, full.segments[2].Start, seek.segments[0].Start)
	assert.Equal(t, full.segments[2].Video.Data, seek.segments[0].Video.Data)
	assert.Equal(t, full.init[0], seek.init[0])
}

// Review focus 2: every audio fragment ends at its segment's video
// boundary, within one AAC frame.
func TestAudioFragmentsEndAtTheVideoBoundaries(t *testing.T) {
	r := run(t, job(clip(t, 20, 48), 1), Options{From: 1, StartAt: -1})
	frame := time.Duration(1024) * time.Second / 48000
	for _, s := range r.segments[:len(r.segments)-1] {
		end := time.Duration(s.Audio.T+s.Audio.D) * time.Second / time.Duration(r.scales[1])
		assert.InDelta(t, float64(s.End), float64(end), float64(frame), "segment %d", s.N)
	}
	var a [][]byte
	for _, s := range r.segments {
		a = append(a, s.Audio.Data)
	}
	assert.Greater(t, decodes(t, r.init[1], a), 0)
}

func TestProgressNamesPlexsKeys(t *testing.T) {
	r := run(t, job(clip(t, 10, 48), 1), Options{From: 1, StartAt: -1})
	joined := strings.Join(r.progress, "\n")
	for _, want := range []string{"stream?index=0", "streamDetail?index=1", "?duration=", "?width=320&height=180", "?progress="} {
		assert.Contains(t, joined, want)
	}
}

func TestACancelledRunStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, job(clip(t, 10, 48), 1), Options{From: 1, StartAt: -1}, &recorder{})
	assert.ErrorIs(t, err, context.Canceled)
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./pkg/remux/pipeline/`
Expected: FAIL to build: `undefined: Run`.

- [ ] **Step 3: Implement the boundary tracker** — `pkg/remux/pipeline/boundaries.go`:

```go
package pipeline

import (
	"context"
	"sync"
	"time"
)

// boundaries is what the video side has learnt of segment starts, shared
// with the audio muxer, which may hold an encoded packet until the video
// has read past its time: an audio packet is demuxed ahead of the keyframe
// that ends its segment (review focus 2), so cutting audio on its own
// clock would cut early.
type boundaries struct {
	mu      sync.Mutex
	cond    *sync.Cond
	starts  []time.Duration // starts[i] begins segment From+i
	horizon time.Duration   // latest video time read
	done    bool            // the video ended; no boundary will come
}

func newBoundaries() *boundaries {
	b := &boundaries{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *boundaries) add(t time.Duration) {
	b.mu.Lock()
	b.starts = append(b.starts, t)
	b.mu.Unlock()
	b.cond.Broadcast()
}

func (b *boundaries) advance(t time.Duration) {
	b.mu.Lock()
	if t > b.horizon {
		b.horizon = t
	}
	b.mu.Unlock()
	b.cond.Broadcast()
}

func (b *boundaries) finish() {
	b.mu.Lock()
	b.done = true
	b.mu.Unlock()
	b.cond.Broadcast()
}

// wait blocks until the video has read past t, or ended, or ctx ends. It
// returns the starts known then.
func (b *boundaries) wait(ctx context.Context, t time.Duration) ([]time.Duration, error) {
	stop := context.AfterFunc(ctx, b.cond.Broadcast)
	defer stop()
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.horizon < t && !b.done {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.cond.Wait()
	}
	return append([]time.Duration(nil), b.starts...), ctx.Err()
}
```

- [ ] **Step 4: Implement the pipeline** — `pkg/remux/pipeline/pipeline.go`:

```go
// Package pipeline is the remux worker's ffgo pipeline: one demux, the
// video copied, the audio converted, each representation muxed into
// fragmented MP4 through an io.Writer and cut into Plex's segments at
// deterministic keyframe boundaries. Only cmd/remux-worker links it.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/obinnaokechukwu/ffgo/avcodec"
	"github.com/obinnaokechukwu/ffgo/avutil"

	"github.com/mediactl/clusterplex/pkg/remux"
)

type Sink interface {
	Init(video, audio []byte, timescales [2]int32, info remux.StreamInfo) error
	Segment(s remux.Segment) error
	Progress(path, query string)
}

type Options struct {
	From    int
	StartAt time.Duration
}

var fragmented = map[string]string{"movflags": "frag_custom+empty_moov+default_base_moof"}

// rep is one representation's muxer and what it has cut.
type rep struct {
	m       *ffgo.Muxer
	ms      *ffgo.MuxerStream
	split   *remux.Splitter
	init    []byte
	frag    []byte // set by the splitter during Flush
	scale   ffgo.Rational
	first   int64 // first timestamp of the open fragment, output time base; -1 none
	end     int64 // end of the last packet written, output time base
}

func newRep() (*rep, error) {
	r := &rep{first: -1}
	r.split = &remux.Splitter{
		OnInit:     func(b []byte) error { r.init = append([]byte(nil), b...); return nil },
		OnFragment: func(b []byte) error { r.frag = append([]byte(nil), b...); return nil },
	}
	m, err := ffgo.NewMuxerToWriter(r.split, "mp4")
	if err != nil {
		return nil, err
	}
	r.m = m
	return r, nil
}

func (r *rep) header() error {
	if err := r.m.WriteHeaderWithOptions(fragmented); err != nil {
		return err
	}
	r.scale = r.ms.OutputTimeBase()
	return r.m.Flush() // the init reaches the splitter
}

// write muxes p, whose timestamps are in tb, and tracks the fragment's span.
func (r *rep) write(p *ffgo.Packet, tb ffgo.Rational) error {
	pts := rescale(p.PTS(), tb, r.scale)
	if r.first < 0 {
		r.first = pts
	}
	if end := pts + rescale(avcodec.GetPacketDuration(p.Raw()), tb, r.scale); end > r.end {
		r.end = end
	}
	return r.m.WritePacket(r.ms, p)
}

// cut ends the open fragment and returns it.
func (r *rep) cut() (remux.Fragment, error) {
	r.frag = nil
	if err := r.m.Flush(); err != nil {
		return remux.Fragment{}, err
	}
	f := remux.Fragment{Data: r.frag, T: r.first, D: r.end - r.first}
	r.first = -1
	return f, nil
}

func rescale(v int64, from, to ffgo.Rational) int64 {
	if v == avutil.AV_NOPTS_VALUE || from.Den == 0 || to.Num == 0 {
		return v
	}
	return v * int64(from.Num) * int64(to.Den) / (int64(from.Den) * int64(to.Num))
}

func toTime(v int64, tb ffgo.Rational) time.Duration {
	return time.Duration(v) * time.Second * time.Duration(tb.Num) / time.Duration(tb.Den)
}

var mpdCodecs = map[string]string{"hevc": "hev1", "h264": "avc1", "av1": "av01"}

// Run remuxes job from segment o.From to the end of the input, handing
// each finished segment to sink in order. It returns the last segment's
// number.
func Run(ctx context.Context, job remux.Job, o Options, sink Sink) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := ffgo.Init(); err != nil {
		return 0, err
	}
	d, err := ffgo.NewDecoder(job.Input)
	if err != nil {
		return 0, err
	}
	defer func() { _ = d.Close() }()
	var vsrc, asrc *ffgo.StreamInfo
	for _, s := range d.Streams() {
		switch s.Index {
		case job.VideoStream:
			vsrc = s
		case job.AudioStream:
			asrc = s
		}
	}
	if vsrc == nil || vsrc.Type != ffgo.MediaTypeVideo || asrc == nil || asrc.Type != ffgo.MediaTypeAudio {
		return 0, fmt.Errorf("%w: streams %d and %d are not video and audio", remux.ErrNotRemux, job.VideoStream, job.AudioStream)
	}
	codec, ok := mpdCodecs[vsrc.CodecName]
	if !ok {
		return 0, fmt.Errorf("%w: video codec %s", remux.ErrNotRemux, vsrc.CodecName)
	}
	shift := map[int]int64{}
	if st := d.StartTime(); st > 0 {
		for _, s := range []*ffgo.StreamInfo{vsrc, asrc} {
			shift[s.Index] = rescale(st, ffgo.NewRational(1, 1000000), s.TimeBase)
		}
	}

	video, err := newRep()
	if err != nil {
		return 0, err
	}
	defer func() { _ = video.m.Close() }()
	if video.ms, err = video.m.AddCopyStream(&ffgo.CopyStreamConfig{CodecParameters: vsrc.CodecParameters(), TimeBase: vsrc.TimeBase}); err != nil {
		return 0, err
	}
	if err := video.header(); err != nil {
		return 0, err
	}
	aud, err := newAudio(job, asrc, d)
	if err != nil {
		return 0, err
	}
	defer aud.close()

	channels, rate := job.AudioChannels, job.AudioSampleRate
	if channels == 0 {
		channels = asrc.Channels
	}
	if rate == 0 {
		rate = asrc.SampleRate
	}
	info := remux.StreamInfo{VideoCodec: codec, AudioCodec: "mp4a.40.2", Width: vsrc.Width, Height: vsrc.Height,
		FrameRate: fmt.Sprintf("%d/%d", vsrc.FrameRate.Num, vsrc.FrameRate.Den),
		SampleRate: rate, Channels: channels, Duration: d.Duration()}
	if err := sink.Init(video.init, aud.rep.init, [2]int32{video.scale.Den, aud.rep.scale.Den}, info); err != nil {
		return 0, err
	}
	announce(sink, vsrc, asrc, info)

	target := time.Duration(o.From-1) * job.SegmentDuration
	startAt := o.StartAt
	if startAt < 0 {
		startAt = target
	}
	if startAt > 0 {
		if err := d.Seek(startAt); err != nil {
			return 0, err
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b := newBoundaries()
	segs := newPairs(sink, o.From)
	var wg sync.WaitGroup
	var audioErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		if audioErr = aud.run(ctx, b, segs); audioErr != nil {
			cancel()
		}
	}()

	began := time.Now()
	n := o.From
	last := time.Duration(-1)
	var demuxErr error
	for {
		if demuxErr = ctx.Err(); demuxErr != nil {
			break
		}
		p, err := d.ReadPacket()
		if err != nil {
			demuxErr = err
			break
		}
		if p == nil {
			break
		}
		idx := p.StreamIndex()
		if idx != job.VideoStream && idx != job.AudioStream {
			continue
		}
		c, err := p.Clone()
		if err != nil {
			demuxErr = err
			break
		}
		if off := shift[idx]; off != 0 {
			if ts := c.PTS(); ts != avutil.AV_NOPTS_VALUE {
				avcodec.SetPacketPTS(c.Raw(), ts-off)
			}
			if ts := c.DTS(); ts != avutil.AV_NOPTS_VALUE {
				avcodec.SetPacketDTS(c.Raw(), ts-off)
			}
		}
		if idx == job.AudioStream {
			aud.queue.push(c)
			continue
		}
		ts := toTime(c.PTS(), vsrc.TimeBase)
		key := avcodec.GetPacketFlags(c.Raw())&avcodec.PacketFlagKey != 0
		switch {
		case last < 0: // dropping up to the first keyframe at or after startAt
			if !key || ts < startAt {
				_ = c.Free()
				continue
			}
			last = ts
			b.add(ts)
		case key && ts >= time.Duration(n)*job.SegmentDuration && ts > last:
			f, err := video.cut()
			if err != nil {
				demuxErr = err
			}
			segs.video(n, f, last, ts)
			n++
			last = ts
			b.add(ts)
			progress(sink, ts, info.Duration, began)
		}
		if demuxErr == nil {
			demuxErr = video.write(c, vsrc.TimeBase)
		}
		_ = c.Free()
		b.advance(ts)
		if demuxErr != nil {
			break
		}
	}
	aud.queue.close()
	if demuxErr == nil && last >= 0 {
		f, err := video.cut()
		if err != nil {
			demuxErr = err
		}
		segs.video(n, f, last, toTime(video.end, video.scale))
	}
	b.finish()
	wg.Wait()
	if err := errors.Join(demuxErr, audioErr, segs.err()); err != nil {
		if cerr := ctx.Err(); cerr != nil && errors.Is(err, cerr) {
			return 0, cerr
		}
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

// announce sends what Plex's binary sends before its first progress.
func announce(sink Sink, v, a *ffgo.StreamInfo, info remux.StreamInfo) {
	sink.Progress("stream", fmt.Sprintf("index=0&id=0&codec=%s&type=video", v.CodecName))
	sink.Progress("stream", fmt.Sprintf("index=1&id=0&codec=%s&type=audio", a.CodecName))
	sink.Progress("streamDetail", fmt.Sprintf("index=0&id=0&codec=%s&type=video&width=%d&height=%d", v.CodecName, v.Width, v.Height))
	sink.Progress("streamDetail", fmt.Sprintf("index=1&id=0&codec=%s&type=audio&channels=%d&sampleRate=%d", a.CodecName, a.Channels, a.SampleRate))
	sink.Progress("", fmt.Sprintf("duration=%f", info.Duration.Seconds()))
	sink.Progress("", fmt.Sprintf("width=%d&height=%d", info.Width, info.Height))
}

func progress(sink Sink, at, total time.Duration, began time.Time) {
	if total <= 0 {
		return
	}
	speed := at.Seconds() / max(time.Since(began).Seconds(), 0.001)
	remaining := (total - at).Seconds() / max(speed, 0.001)
	sink.Progress("", fmt.Sprintf("progress=%.1f&size=-1&remaining=%d&speed=%.1f", 100*at.Seconds()/total.Seconds(), int(remaining), speed))
}

// pairs joins each segment's video and audio fragments and hands whole
// segments to the sink in order.
type pairs struct {
	mu      sync.Mutex
	sink    Sink
	next    int
	pending map[int]*remux.Segment
	have    map[int]int
	e       error
}

func newPairs(sink Sink, from int) *pairs {
	return &pairs{sink: sink, next: from, pending: map[int]*remux.Segment{}, have: map[int]int{}}
}

func (p *pairs) video(n int, f remux.Fragment, start, end time.Duration) {
	p.put(n, func(s *remux.Segment) { s.Video, s.Start, s.End = f, start, end })
}

func (p *pairs) audio(n int, f remux.Fragment) { p.put(n, func(s *remux.Segment) { s.Audio = f }) }

func (p *pairs) put(n int, set func(*remux.Segment)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.pending[n]
	if !ok {
		s = &remux.Segment{N: n}
		p.pending[n] = s
	}
	set(s)
	p.have[n]++
	for p.have[p.next] == 2 {
		s := p.pending[p.next]
		delete(p.pending, p.next)
		delete(p.have, p.next)
		p.next++
		if p.e == nil {
			p.e = p.sink.Segment(*s)
		}
	}
}

func (p *pairs) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.e
}

// queue is the audio packets the demuxer read, unbounded so the demuxer
// never waits on the audio side (which waits on the demuxer's horizon).
type queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []*ffgo.Packet
	closed bool
}

func newQueue() *queue {
	q := &queue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *queue) push(p *ffgo.Packet) {
	q.mu.Lock()
	q.items = append(q.items, p)
	q.mu.Unlock()
	q.cond.Signal()
}

func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// pop returns the next packet, nil once closed and drained or ctx ends.
func (q *queue) pop(ctx context.Context) *ffgo.Packet {
	stop := context.AfterFunc(ctx, q.cond.Broadcast)
	defer stop()
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed && ctx.Err() == nil {
		q.cond.Wait()
	}
	if len(q.items) == 0 || ctx.Err() != nil {
		return nil
	}
	p := q.items[0]
	q.items = q.items[1:]
	return p
}

func (q *queue) drain() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, p := range q.items {
		_ = p.Free()
	}
	q.items = nil
}

// audio decodes, resamples and encodes (or copies) the audio stream into
// its representation, cutting at the video's boundaries.
type audio struct {
	rep   *rep
	queue *queue
	src   *ffgo.StreamInfo
	copy  bool
	sd    *ffgo.StreamDecoder
	enc   *ffgo.AudioEncoder
	res   *ffgo.Resampler
	rate  int
	lay   string
	from  int
}

func newAudio(job remux.Job, src *ffgo.StreamInfo, d *ffgo.Decoder) (*audio, error) {
	r, err := newRep()
	if err != nil {
		return nil, err
	}
	a := &audio{rep: r, queue: newQueue(), src: src, copy: job.AudioCopy, from: job.SkipToSegment}
	if a.copy {
		if r.ms, err = r.m.AddCopyStream(&ffgo.CopyStreamConfig{CodecParameters: src.CodecParameters(), TimeBase: src.TimeBase,
			Options: ffgo.StreamOptions{Language: job.AudioLanguage}}); err != nil {
			return nil, err
		}
		return a, r.header()
	}
	a.rate = job.AudioSampleRate
	if a.rate == 0 {
		a.rate = src.SampleRate
	}
	a.lay = "stereo"
	if job.AudioChannels != 2 {
		a.lay = src.ChannelLayout
	}
	if a.sd, err = d.NewStreamDecoder(src.Index, nil); err != nil {
		return nil, err
	}
	if a.enc, err = ffgo.NewAudioEncoder(ffgo.AudioEncoderConfig2{SampleRate: a.rate, Layout: a.lay, BitRate: job.AudioBitRate,
		GlobalHeader: true, InputTimeBase: a.sd.TimeBase()}); err != nil {
		return nil, err
	}
	if r.ms, err = r.m.AddEncoderStream(a.enc, ffgo.StreamOptions{Language: job.AudioLanguage}); err != nil {
		return nil, err
	}
	return a, r.header()
}

func (a *audio) close() {
	a.queue.drain()
	if a.res != nil {
		_ = a.res.Close()
	}
	if a.enc != nil {
		_ = a.enc.Close()
	}
	if a.sd != nil {
		_ = a.sd.Close()
	}
	_ = a.rep.m.Close()
}

// run consumes the queue until the demuxer closes it.
func (a *audio) run(ctx context.Context, b *boundaries, segs *pairs) error {
	n := a.from
	tb := a.src.TimeBase
	if !a.copy {
		tb = a.enc.TimeBase()
	}
	emit := func(p *ffgo.Packet) error {
		t := toTime(p.PTS(), tb)
		starts, err := b.wait(ctx, t)
		if err != nil {
			return err
		}
		if len(starts) == 0 || t < starts[0] {
			return nil // before the first segment's keyframe
		}
		for i := n - a.from + 1; i < len(starts) && t >= starts[i]; i = n - a.from + 1 {
			f, err := a.rep.cut()
			if err != nil {
				return err
			}
			segs.audio(n, f)
			n++
		}
		return a.rep.write(p, tb)
	}
	encode := func(p *ffgo.Packet) error {
		if a.copy {
			return emit(p)
		}
		return a.decode(p, emit)
	}
	for {
		p := a.queue.pop(ctx)
		if p == nil {
			break
		}
		err := encode(p)
		_ = p.Free()
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !a.copy {
		if err := a.decode(nil, emit); err != nil {
			return err
		}
		if a.res != nil {
			if tail, err := a.res.Flush(); err == nil && !tail.IsNil() {
				err := a.enc.Encode(tail, emit)
				_ = tail.Free()
				if err != nil {
					return err
				}
			}
		}
		if err := a.enc.Flush(emit); err != nil {
			return err
		}
	}
	f, err := a.rep.cut()
	if err != nil {
		return err
	}
	segs.audio(n, f)
	return nil
}

// decode sends p (nil at the end) and encodes every frame it yields.
func (a *audio) decode(p *ffgo.Packet, emit func(*ffgo.Packet) error) error {
	for {
		err := a.sd.Send(p)
		if err != nil && !errors.Is(err, ffgo.ErrAgain) {
			return err
		}
		for {
			f, rerr := a.sd.Receive()
			if errors.Is(rerr, ffgo.ErrAgain) || errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				return rerr
			}
			if a.res == nil {
				if a.res, rerr = ffgo.NewResampler(
					ffgo.AudioFormat{SampleRate: a.src.SampleRate, Layout: f.ChannelLayout(), SampleFormat: ffgo.SampleFormat(f.Format())},
					ffgo.AudioFormat{SampleRate: a.rate, Layout: a.lay, SampleFormat: ffgo.SampleFormatFLTP}); rerr != nil {
					return rerr
				}
			}
			r, rerr := a.res.Resample(f)
			if rerr != nil {
				return rerr
			}
			if r.IsNil() {
				continue
			}
			r.SetPTS(f.PTS())
			rerr = a.enc.Encode(r, emit)
			_ = r.Free()
			if rerr != nil {
				return rerr
			}
		}
		if !errors.Is(err, ffgo.ErrAgain) {
			return nil
		}
	}
}
```

- [ ] **Step 5: Tidy and run the tests**

Run: `go mod tidy && go test -count=1 ./pkg/remux/pipeline/`
Expected: PASS. If the starts differ, compare them with the clip's keyframes (`ffprobe -select_streams v -show_entries packet=pts_time,flags -of csv clip.mkv`): the rule is the spec's, so fix the pipeline, not the expected list.

- [ ] **Step 6: Falsify** — comment out `b.add(ts)` in the boundary case and run `go test -run TestAudioFragmentsEndAtTheVideoBoundaries ./pkg/remux/pipeline/`.
Expected: FAIL (audio never cuts). Restore.

- [ ] **Step 7: Commit**

```bash
git commit -m "feat(remux): the ffgo pipeline -- one demux, video copied, audio to stereo AAC, each representation muxed into fragmented MP4 through an io.Writer and cut at deterministic keyframe boundaries" -- go.mod go.sum pkg/remux/pipeline/pipeline.go pkg/remux/pipeline/boundaries.go pkg/remux/pipeline/pipeline_test.go
```

---

### Task 9: `pkg/remux/worker` and `cmd/remux-worker`

**Files:**
- Create: `pkg/remux/worker/server.go`, `pkg/remux/worker/server_test.go`
- Create: `cmd/remux-worker/main.go`
- Modify: `Makefile` (`build` builds `bin/remux-worker`)

**Interfaces:**
- Consumes: `pipeline.Run`, `pipeline.Sink`, `cache.Cache`, `remux.Manifest`, `remuxpb.RemuxServer`.
- Produces: `type Server struct { Cache *cache.Cache; Run func(ctx context.Context, job remux.Job, o pipeline.Options, sink pipeline.Sink) (int, error); Now func() time.Time; Logger *slog.Logger }`, which implements `remuxpb.RemuxServer`.

The server's flow for a job at segment `n`:
1. Stat the input; open the cache entry for `cache.Key{...}`.
2. If the entry has an init and segment `n` whose start is where the rule puts it (or `n` continues a chain), send the inits, then cached segments while each starts where the previous ended; a manifest after each.
3. At the first gap (or no cache): run the pipeline from that segment with `StartAt` = the previous segment's `End` (or -1 at the start). Its `Init` is compared to the cache's (`SetInit` resets on a difference) and sent only if not already sent; each `Segment` is put in the cache and sent, then a manifest.
4. At the end: `SetLast`, a final static manifest, `Done{}`. On error: `Done{error}`.

- [ ] **Step 1: Write the failing tests** — `pkg/remux/worker/server_test.go` (a fake `Run` that emits synthetic segments, so this tests the server's flow, not ffgo):

```go
package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/mediactl/clusterplex/pkg/remux"
	"github.com/mediactl/clusterplex/pkg/remux/cache"
	"github.com/mediactl/clusterplex/pkg/remux/pipeline"
	"github.com/mediactl/clusterplex/proto/remuxpb"
)

type stream struct {
	grpc.ServerStream
	ctx    context.Context
	events []*remuxpb.Event
}

func (s *stream) Context() context.Context { return s.ctx }
func (s *stream) Send(e *remuxpb.Event) error {
	s.events = append(s.events, e)
	return nil
}

// fakeRun emits segments from o.From to 4, each 5 s long, and counts runs.
type fakeRun struct{ runs, decoded int }

func (f *fakeRun) Run(_ context.Context, _ remux.Job, o pipeline.Options, sink pipeline.Sink) (int, error) {
	f.runs++
	if err := sink.Init([]byte("vinit"), []byte("ainit"), [2]int32{12288, 48000}, remux.StreamInfo{VideoCodec: "hev1", AudioCodec: "mp4a.40.2", Channels: 2}); err != nil {
		return 0, err
	}
	for n := o.From; n <= 4; n++ {
		f.decoded++
		start := time.Duration(n-1) * 5 * time.Second
		if err := sink.Segment(remux.Segment{N: n, Start: start, End: start + 5*time.Second,
			Video: remux.Fragment{Data: []byte{'v', byte(n)}, T: int64(n-1) * 61440, D: 61440},
			Audio: remux.Fragment{Data: []byte{'a', byte(n)}, T: int64(n-1) * 240000, D: 240000}}); err != nil {
			return 0, err
		}
	}
	return 4, nil
}

func input(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "a.mkv")
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	return p
}

func files(s *stream) []string {
	var out []string
	for _, e := range s.events {
		switch k := e.Kind.(type) {
		case *remuxpb.Event_File:
			out = append(out, k.File.Name)
		case *remuxpb.Event_Manifest:
			out = append(out, "manifest")
		case *remuxpb.Event_Done:
			out = append(out, "done:"+k.Done.Error)
		}
	}
	return out
}

func TestAFreshJobRunsThePipelineAndEveryFileComesBeforeItsManifest(t *testing.T) {
	f := &fakeRun{}
	srv := &Server{Cache: &cache.Cache{Dir: t.TempDir()}, Run: f.Run}
	s := &stream{ctx: t.Context()}
	j := remux.Job{Input: input(t), SkipToSegment: 1, SegmentDuration: 5 * time.Second, AudioStream: 1, AudioChannels: 2}
	require.NoError(t, srv.Remux(j.Proto(), s))
	assert.Equal(t, []string{
		"init-stream0.m4s", "init-stream1.m4s",
		"chunk-stream0-00001.m4s", "chunk-stream1-00001.m4s", "manifest",
		"chunk-stream0-00002.m4s", "chunk-stream1-00002.m4s", "manifest",
		"chunk-stream0-00003.m4s", "chunk-stream1-00003.m4s", "manifest",
		"chunk-stream0-00004.m4s", "chunk-stream1-00004.m4s", "manifest",
		"manifest", "done:",
	}, files(s))
	last := s.events[len(s.events)-2].GetManifest().GetMpd()
	assert.Contains(t, string(last), `type="static"`)
}

func TestAReplayIsServedFromTheCacheWithoutDecoding(t *testing.T) {
	f := &fakeRun{}
	srv := &Server{Cache: &cache.Cache{Dir: t.TempDir()}, Run: f.Run}
	j := remux.Job{Input: input(t), SkipToSegment: 1, SegmentDuration: 5 * time.Second, AudioStream: 1, AudioChannels: 2}
	require.NoError(t, srv.Remux(j.Proto(), &stream{ctx: t.Context()}))
	s := &stream{ctx: t.Context()}
	require.NoError(t, srv.Remux(j.Proto(), s))
	assert.Equal(t, 1, f.runs, "the second play never runs the pipeline")
	assert.Equal(t, files(&stream{events: s.events}), files(s))
	assert.True(t, strings.HasPrefix(files(s)[len(files(s))-1], "done:"))
}

func TestAGapIsFilledFromTheEndOfTheLastCachedSegment(t *testing.T) {
	var got pipeline.Options
	f := &fakeRun{}
	srv := &Server{Cache: &cache.Cache{Dir: t.TempDir()}, Run: func(ctx context.Context, j remux.Job, o pipeline.Options, sink pipeline.Sink) (int, error) {
		got = o
		return f.Run(ctx, j, o, sink)
	}}
	in := input(t)
	j := remux.Job{Input: in, SkipToSegment: 1, SegmentDuration: 5 * time.Second, AudioStream: 1, AudioChannels: 2}
	e, err := srv.Cache.Open(srv.key(j, mustStat(t, in)))
	require.NoError(t, err)
	require.NoError(t, e.SetInit([]byte("vinit"), []byte("ainit"), [2]int32{12288, 48000}, remux.StreamInfo{}))
	require.NoError(t, e.Put(remux.Segment{N: 1, End: 5 * time.Second, Video: remux.Fragment{Data: []byte("v1")}, Audio: remux.Fragment{Data: []byte("a1")}}))
	require.NoError(t, srv.Remux(j.Proto(), &stream{ctx: t.Context()}))
	assert.Equal(t, pipeline.Options{From: 2, StartAt: 5 * time.Second}, got)
}
```

(`mustStat` is a test helper returning `os.Stat`'s `FileInfo`; `srv.key` is the server's unexported `func (s *Server) key(j remux.Job, fi os.FileInfo) cache.Key`.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./pkg/remux/worker/`
Expected: FAIL to build: `undefined: Server`.

- [ ] **Step 3: Implement** — `pkg/remux/worker/server.go`:

```go
// Package worker serves the Remux gRPC service on the remux pool: the
// cache first, the ffgo pipeline from the first gap.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mediactl/clusterplex/pkg/remux"
	"github.com/mediactl/clusterplex/pkg/remux/cache"
	"github.com/mediactl/clusterplex/pkg/remux/pipeline"
	"github.com/mediactl/clusterplex/proto/remuxpb"
)

type Server struct {
	remuxpb.UnimplementedRemuxServer
	Cache  *cache.Cache
	Run    func(ctx context.Context, job remux.Job, o pipeline.Options, sink pipeline.Sink) (int, error)
	Now    func() time.Time
	Logger *slog.Logger
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) key(j remux.Job, fi os.FileInfo) cache.Key {
	return cache.Key{Input: j.Input, Size: fi.Size(), ModTime: fi.ModTime(), AudioStream: j.AudioStream,
		AudioCopy: j.AudioCopy, Channels: j.AudioChannels, SampleRate: j.AudioSampleRate,
		BitRate: j.AudioBitRate, SegmentDuration: j.SegmentDuration}
}

// session is one job's stream: what was sent and the manifest's timelines.
type session struct {
	srv      *Server
	out      remuxpb.Remux_RemuxServer
	job      remux.Job
	entry    *cache.Entry
	start    time.Time
	sentInit bool
	scales   [2]int32
	info     remux.StreamInfo
	manifest remux.Manifest
	end      time.Duration
}

func (s *Server) Remux(p *remuxpb.Job, out remuxpb.Remux_RemuxServer) error {
	j := remux.JobFromProto(p)
	err := s.remux(out.Context(), j, out)
	msg := ""
	if err != nil {
		msg = err.Error()
		if s.Logger != nil {
			s.Logger.Warn("remux failed", "input", j.Input, "segment", j.SkipToSegment, "error", err)
		}
	}
	return out.Send(&remuxpb.Event{Kind: &remuxpb.Event_Done{Done: &remuxpb.Done{Error: msg}}})
}

func (s *Server) remux(ctx context.Context, j remux.Job, out remuxpb.Remux_RemuxServer) error {
	fi, err := os.Stat(j.Input)
	if err != nil {
		return err
	}
	e, err := s.Cache.Open(s.key(j, fi))
	if err != nil {
		return err
	}
	ss := &session{srv: s, out: out, job: j, entry: e, start: s.now(),
		manifest: remux.Manifest{StartNumber: j.SkipToSegment, SegmentDuration: j.SegmentDuration, Start: s.now()}}
	n, startAt := j.SkipToSegment, time.Duration(-1)
	if v, a, ts, info, ok := e.Init(); ok {
		if seg, ok, err := e.Segment(n); err != nil {
			return err
		} else if ok && (n == 1 && seg.Start == 0 || n > 1) {
			// A segment from a run that started elsewhere has the start the
			// rule gives only when that run reached it from the same
			// keyframe; a chain from the cache is trusted from n onwards.
			ss.info = info
			ss.manifest.Duration = info.Duration
			if err := ss.sendInit(v, a, ts, info); err != nil {
				return err
			}
			for ok {
				if err := ss.Segment(seg); err != nil {
					return err
				}
				n, startAt = n+1, seg.End
				next, found, err := e.Segment(n)
				if err != nil {
					return err
				}
				ok = found && next.Start == seg.End
				seg = next
			}
			if last := e.Last(); last > 0 && n > last {
				return ss.finish()
			}
		}
	}
	last, err := s.Run(ctx, j, pipeline.Options{From: n, StartAt: startAt}, ss)
	if err != nil {
		return err
	}
	if err := e.SetLast(last); err != nil {
		return err
	}
	_ = s.Cache.Evict()
	return ss.finish()
}

func (ss *session) send(e *remuxpb.Event) error { return ss.out.Send(e) }

func (ss *session) file(name string, data []byte) error {
	return ss.send(&remuxpb.Event{Kind: &remuxpb.Event_File{File: &remuxpb.File{Name: name, Data: data}}})
}

func (ss *session) sendInit(v, a []byte, ts [2]int32, info remux.StreamInfo) error {
	if ss.sentInit {
		return nil
	}
	ss.sentInit, ss.scales = true, ts
	ss.manifest.Video = remux.Representation{Codecs: info.VideoCodec, Width: info.Width, Height: info.Height, FrameRate: info.FrameRate, Timescale: ts[0]}
	ss.manifest.Audio = remux.Representation{Codecs: "mp4a.40.2", SampleRate: info.SampleRate, Channels: info.Channels, Language: ss.job.AudioLanguage, Timescale: ts[1]}
	if err := ss.file(remux.InitFile(0), v); err != nil {
		return err
	}
	return ss.file(remux.InitFile(1), a)
}

// Init implements pipeline.Sink: the cache keeps the inits (resetting an
// entry muxed against others), the client gets them once.
func (ss *session) Init(v, a []byte, ts [2]int32, info remux.StreamInfo) error {
	ss.info = info
	if err := ss.entry.SetInit(v, a, ts, info); err != nil {
		return err
	}
	if ss.sentInit && ts != ss.scales {
		return errors.New("remux: the cached inits' timescales differ from this run's")
	}
	ss.manifest.Duration = info.Duration
	return ss.sendInit(v, a, ts, info)
}

// Segment implements pipeline.Sink: into the cache, then to the client,
// files before the manifest that lists them.
func (ss *session) Segment(s remux.Segment) error {
	if err := ss.entry.Put(s); err != nil {
		return err
	}
	if err := ss.file(remux.ChunkFile(0, s.N), s.Video.Data); err != nil {
		return err
	}
	if err := ss.file(remux.ChunkFile(1, s.N), s.Audio.Data); err != nil {
		return err
	}
	ss.manifest.Video.Timeline = append(ss.manifest.Video.Timeline, remux.TimelineEntry{T: s.Video.T, D: s.Video.D})
	ss.manifest.Audio.Timeline = append(ss.manifest.Audio.Timeline, remux.TimelineEntry{T: s.Audio.T, D: s.Audio.D})
	ss.end = s.End
	ss.manifest.Now = ss.srv.now()
	return ss.send(&remuxpb.Event{Kind: &remuxpb.Event_Manifest{Manifest: &remuxpb.Manifest{Mpd: ss.manifest.Render()}}})
}

// Progress implements pipeline.Sink.
func (ss *session) Progress(path, query string) {
	_ = ss.send(&remuxpb.Event{Kind: &remuxpb.Event_Progress{Progress: &remuxpb.Progress{Path: path, Query: query}}})
}

func (ss *session) finish() error {
	ss.manifest.Final = true
	if ss.manifest.Duration == 0 {
		ss.manifest.Duration = ss.end
	}
	if err := ss.send(&remuxpb.Event{Kind: &remuxpb.Event_Manifest{Manifest: &remuxpb.Manifest{Mpd: ss.manifest.Render()}}}); err != nil {
		return fmt.Errorf("final manifest: %w", err)
	}
	return nil
}
```

A replay from the cache publishes its manifest from the `StreamInfo` the cache kept with the inits (Task 5), so no pipeline needs to run for it.

Add `mustStat` to the test file:
```go
func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	require.NoError(t, err)
	return fi
}
```

- [ ] **Step 4: Write `cmd/remux-worker/main.go`**

```go
// Command remux-worker serves cluster-plex's Remux gRPC service on the
// remux pool: Plex Web's DASH streams, remuxed in process with ffgo
// (docs/superpowers/specs/2026-10-01-browser-remux-transcoder-design.md).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"

	"github.com/mediactl/clusterplex/pkg/remux/cache"
	"github.com/mediactl/clusterplex/pkg/remux/pipeline"
	"github.com/mediactl/clusterplex/pkg/remux/worker"
	"github.com/mediactl/clusterplex/proto/remuxpb"
)

func main() {
	if err := run(); err != nil {
		slog.Error("remux-worker", "error", err)
		os.Exit(1)
	}
}

func run() error {
	listen := pflag.String("listen", ":50052", "address serving the Remux gRPC service")
	probe := pflag.String("probe", ":8080", "address serving /healthz and /readyz")
	dir := pflag.String("cache-dir", "/cache", "worker-local directory holding the remux cache")
	maxBytes := pflag.Int64("cache-max-bytes", 50<<30, "size the cache is evicted down to; 0 keeps everything")
	pflag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if err := ffgo.Init(); err != nil {
		return fmt.Errorf("load FFmpeg: %w", err)
	}
	if _, avc, _ := ffgo.Version(); avc>>16 != 63 {
		return fmt.Errorf("libavcodec %d is not FFmpeg 9's (63)", avc>>16)
	}
	if d := ffgo.Diagnose(); !d.ShimLoaded {
		return fmt.Errorf("no ffgo shim for FFmpeg 9 loaded: %s", d.ShimError)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	srv := grpc.NewServer()
	remuxpb.RegisterRemuxServer(srv, &worker.Server{
		Cache:  &cache.Cache{Dir: *dir, MaxBytes: *maxBytes},
		Run:    pipeline.Run,
		Logger: log,
	})
	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/readyz", ok)
	hs := &http.Server{Addr: *probe, Handler: mux}
	go func() { _ = hs.ListenAndServe() }()
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
		_ = hs.Close()
	}()
	log.Info("serving", "listen", *listen, "cache", *dir)
	if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}
```

In `Makefile`'s `build` target add `go build -o bin/remux-worker ./cmd/remux-worker`.

- [ ] **Step 5: Run the tests and build**

Run: `go test ./pkg/remux/... && make build`
Expected: PASS; `bin/remux-worker` exists.

- [ ] **Step 6: Commit**

```bash
git commit -m "feat(remux): the remux worker -- the cache first, the ffgo pipeline from the first gap, every file before the manifest that lists it" -- Makefile cmd/remux-worker/main.go pkg/remux/worker
```

---

### Task 10: the manager — relay, dispatcher hook, config, no-ffgo guard

**Files:**
- Create: `pkg/remux/relay/relay.go`, `pkg/remux/relay/relay_test.go`
- Modify: `pkg/remoteexec/dispatcher.go` (a `Remux` hook), `pkg/remoteexec/dispatcher_test.go`
- Modify: `cmd/manager/config.go`, `cmd/manager/execserver.go`, `cmd/manager/config_test.go`
- Create: `cmd/manager/noffgo_test.go`

**Interfaces:**
- Consumes: `remux.Classify`, `remux.Players` (`*plexapi.Client`), `remuxpb.RemuxClient`, `remoteexec.WorkerLister`, `hashring.New(...).Locate(key)`.
- Produces:
```go
// in pkg/remoteexec
type RemuxRoute interface {
	// Execute runs req when it is a browser remux; done=false leaves it to Plex.
	Execute(ctx context.Context, req *pb.ExecRequest, sink Sink) (done bool, err error)
}
// Dispatcher gains: Remux RemuxRoute
// in pkg/remux/relay
type Relay struct {
	Players remux.Players
	Workers remoteexec.WorkerLister
	Dial    func(ctx context.Context, addr string) (remuxpb.RemuxClient, io.Closer, error)
	PMS     string        // http://<Plex's own address>, the manager's plexAddr
	HTTP    *http.Client
	Logger  *slog.Logger
}
func (r *Relay) Execute(ctx context.Context, req *pb.ExecRequest, sink remoteexec.Sink) (bool, error)
```

`relay` imports `pkg/remoteexec` for `Sink` and `WorkerLister`; `remoteexec` must not import `relay` (it holds only the interface).

- [ ] **Step 1: Write the failing relay tests** — `pkg/remux/relay/relay_test.go`:

```go
package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/mediactl/clusterplex/pkg/remoteexec"
	"github.com/mediactl/clusterplex/proto/remuxpb"
	pb "github.com/mediactl/clusterplex/proto"
)

// pms records what reaches Plex and, for each manifest, which files the
// session directory held at that moment.
type pms struct {
	mu        sync.Mutex
	dir       string
	calls     []string
	tokens    []string
	atPublish [][]string
}

func (p *pms) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	p.tokens = append(p.tokens, r.Header.Get("X-Plex-Token"))
	if r.Method == http.MethodPost {
		ents, _ := os.ReadDir(p.dir)
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		p.atPublish = append(p.atPublish, names)
	}
}

type players struct{}

func (players) PlayerProduct(context.Context, string) (string, error) { return "Plex Web", nil }

type workers []remoteexec.Worker

func (w workers) ListReady(context.Context) ([]remoteexec.Worker, error) { return w, nil }

type events struct {
	grpc.ClientStream
	evs []*remuxpb.Event
	err error // returned after evs
}

func (e *events) Recv() (*remuxpb.Event, error) {
	if len(e.evs) == 0 {
		if e.err != nil {
			return nil, e.err
		}
		return nil, io.EOF
	}
	ev := e.evs[0]
	e.evs = e.evs[1:]
	return ev, nil
}
func (e *events) Header() (metadata.MD, error) { return nil, nil }

type client struct{ s *events }

func (c client) Remux(context.Context, *remuxpb.Job, ...grpc.CallOption) (remuxpb.Remux_RemuxClient, error) {
	return c.s, nil
}

type sink struct{ logs []*pb.TranscodeLog }

func (s *sink) Send(m *pb.TranscodeLog) error { s.logs = append(s.logs, m); return nil }

func file(name string) *remuxpb.Event {
	return &remuxpb.Event{Kind: &remuxpb.Event_File{File: &remuxpb.File{Name: name, Data: []byte(name)}}}
}
func manifest() *remuxpb.Event {
	return &remuxpb.Event{Kind: &remuxpb.Event_Manifest{Manifest: &remuxpb.Manifest{Mpd: []byte("<MPD/>")}}}
}
func done(msg string) *remuxpb.Event {
	return &remuxpb.Event{Kind: &remuxpb.Event_Done{Done: &remuxpb.Done{Error: msg}}}
}

const argv = `-codec:0 hevc -codec:1 aac -i /m/a.mkv -progressurl http://127.0.0.1:32400/video/:/transcode/session/s1/u1/progress -map 0:0 -codec:0 copy -filter_complex "[0:1] aresample=async=1:ochl='stereo':rematrix_maxval=0.000000dB:osr=48000[0]" -map "[0]" -codec:1 aac -b:1 256k -f dash -seg_duration 5 -dash_segment_type mp4 -init_seg_name 'init-stream$RepresentationID$.m4s' -media_seg_name 'chunk-stream$RepresentationID$-$Number%05d$.m4s' -skip_to_segment 1 -manifest_name "http://127.0.0.1:32400/video/:/transcode/session/s1/u1/manifest?X-Plex-Http-Pipeline=infinite" dash`

func setup(t *testing.T, evs []*remuxpb.Event, streamErr error) (*Relay, *pms, *pb.ExecRequest) {
	t.Helper()
	dir := t.TempDir()
	p := &pms{dir: dir}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	r := &Relay{
		Players: players{}, Workers: workers{{Name: "remux-0", Addr: "10.0.0.1:50052"}},
		Dial: func(context.Context, string) (remuxpb.RemuxClient, io.Closer, error) {
			return client{&events{evs: evs, err: streamErr}}, io.NopCloser(nil), nil
		},
		PMS: srv.URL, HTTP: srv.Client(),
	}
	return r, p, &pb.ExecRequest{TargetBinary: "Plex Transcoder", Args: splitArgv(argv), Env: map[string]string{"X_PLEX_TOKEN": "tok"}, Cwd: dir}
}

func TestFilesAreWrittenBeforeTheManifestThatListsThem(t *testing.T) {
	r, p, req := setup(t, []*remuxpb.Event{
		file("init-stream0.m4s"), file("init-stream1.m4s"),
		{Kind: &remuxpb.Event_Progress{Progress: &remuxpb.Progress{Path: "", Query: "duration=30.0"}}},
		file("chunk-stream0-00001.m4s"), file("chunk-stream1-00001.m4s"), manifest(), done(""),
	}, nil)
	s := &sink{}
	handled, err := r.Execute(t.Context(), req, s)
	require.True(t, handled)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"PUT /video/:/transcode/session/s1/u1/progress?duration=30.0",
		"POST /video/:/transcode/session/s1/u1/manifest?X-Plex-Http-Pipeline=infinite",
	}, p.calls)
	assert.Equal(t, []string{"tok", "tok"}, p.tokens)
	assert.ElementsMatch(t, []string{"init-stream0.m4s", "init-stream1.m4s", "chunk-stream0-00001.m4s", "chunk-stream1-00001.m4s"}, p.atPublish[0])
	require.NotEmpty(t, s.logs)
	assert.True(t, s.logs[len(s.logs)-1].GetIsFinished())
	assert.Zero(t, s.logs[len(s.logs)-1].GetExitCode())
}

func TestANameOutsidePlexsPatternsIsRefused(t *testing.T) {
	r, _, req := setup(t, []*remuxpb.Event{file("../escape"), done("")}, nil)
	s := &sink{}
	handled, err := r.Execute(t.Context(), req, s)
	assert.True(t, handled)
	assert.Error(t, err)
	_, statErr := os.Stat(filepath.Join(filepath.Dir(req.Cwd), "escape"))
	assert.True(t, os.IsNotExist(statErr))
}

// Review focus 4.
func TestAFailedWorkerAfterOutputExitsNonZero(t *testing.T) {
	r, _, req := setup(t, []*remuxpb.Event{file("init-stream0.m4s")}, errors.New("worker gone"))
	s := &sink{}
	handled, err := r.Execute(t.Context(), req, s)
	assert.True(t, handled, "output was written: Plex must not also run the job")
	assert.Error(t, err)
	last := s.logs[len(s.logs)-1]
	assert.True(t, last.GetIsFinished())
	assert.NotZero(t, last.GetExitCode())
}

func TestAWorkerThatFailsBeforeOutputLeavesTheJobToPlex(t *testing.T) {
	r, _, req := setup(t, nil, errors.New("refused"))
	handled, err := r.Execute(t.Context(), req, &sink{})
	assert.False(t, handled)
	assert.NoError(t, err)
}

func TestNoWorkerLeavesTheJobToPlex(t *testing.T) {
	r, _, req := setup(t, nil, nil)
	r.Workers = workers{}
	handled, _ := r.Execute(t.Context(), req, &sink{})
	assert.False(t, handled)
}
```

Add `splitArgv` to the test file (test helpers are package-local, so this is Task 2's `words`, copied):
```go
func splitArgv(line string) []string {
	var out []string
	var cur strings.Builder
	in, quote := false, byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			if c == '"' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == ' ':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}
```
and `"strings"` to its imports.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./pkg/remux/relay/`
Expected: FAIL to build: `undefined: Relay`.

- [ ] **Step 3: Implement** — `pkg/remux/relay/relay.go`:

```go
// Package relay is the manager's side of the remux pool: it hands a
// browser remux to the worker the hash ring names, writes the segments it
// streams into Plex's session directory, and only then tells Plex about
// them. It links no ffgo.
package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"

	"github.com/mediactl/clusterplex/pkg/hashring"
	"github.com/mediactl/clusterplex/pkg/remoteexec"
	"github.com/mediactl/clusterplex/pkg/remux"
	pb "github.com/mediactl/clusterplex/proto"
	"github.com/mediactl/clusterplex/proto/remuxpb"
)

// segmentName is the only file a worker may name.
var segmentName = regexp.MustCompile(`^(init-stream[01]|chunk-stream[01]-\d{5,})\.m4s$`)

type Relay struct {
	Players remux.Players
	Workers remoteexec.WorkerLister
	Dial    func(ctx context.Context, addr string) (remuxpb.RemuxClient, io.Closer, error)
	PMS     string
	HTTP    *http.Client
	Logger  *slog.Logger
}

func (r *Relay) log() *slog.Logger {
	if r.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Logger
}

func (r *Relay) client() *http.Client {
	if r.HTTP == nil {
		return http.DefaultClient
	}
	return r.HTTP
}

func (r *Relay) Execute(ctx context.Context, req *pb.ExecRequest, sink remoteexec.Sink) (bool, error) {
	job, ok := remux.Classify(ctx, req, r.Players)
	if !ok {
		return false, nil
	}
	ws, err := r.Workers.ListReady(ctx)
	if err != nil || len(ws) == 0 {
		return false, nil
	}
	names := make([]string, len(ws))
	addr := map[string]string{}
	for i, w := range ws {
		names[i], addr[w.Name] = w.Name, w.Addr
	}
	pick := hashring.New(names...).Locate(job.RingKey())
	log := r.log().With("worker", pick, "session", job.SessionID(), "segment", job.SkipToSegment)

	c, closer, err := r.Dial(ctx, addr[pick])
	if err != nil {
		log.Warn("dial remux worker failed; Plex runs the job", "error", err)
		return false, nil
	}
	defer func() { _ = closer.Close() }()
	stream, err := c.Remux(ctx, job.Proto())
	if err != nil {
		log.Warn("remux worker refused the job; Plex runs it", "error", err)
		return false, nil
	}
	first := true
	for {
		ev, err := stream.Recv()
		if err != nil {
			if first {
				log.Warn("remux worker failed before output; Plex runs the job", "error", err)
				return false, nil
			}
			if errors.Is(err, io.EOF) {
				err = errors.New("remux worker ended the stream without Done")
			}
			return true, finish(sink, err)
		}
		first = false
		switch k := ev.Kind.(type) {
		case *remuxpb.Event_File:
			if err := writeFile(req.GetCwd(), k.File.GetName(), k.File.GetData()); err != nil {
				return true, finish(sink, err)
			}
		case *remuxpb.Event_Manifest:
			if err := r.send(ctx, http.MethodPost, job.ManifestURL, "", "", k.Manifest.GetMpd(), job.Token); err != nil {
				return true, finish(sink, fmt.Errorf("post manifest: %w", err))
			}
		case *remuxpb.Event_Progress:
			// Plex's own transcoder carries on past a failed progress PUT too.
			_ = r.send(ctx, http.MethodPut, job.ProgressURL, k.Progress.GetPath(), k.Progress.GetQuery(), nil, job.Token)
		case *remuxpb.Event_Done:
			var err error
			if msg := k.Done.GetError(); msg != "" {
				err = errors.New(msg)
			}
			return true, finish(sink, err)
		}
	}
}

// finish tells the shim how the job ended, as Plex's transcoder's exit code.
func finish(sink remoteexec.Sink, err error) error {
	code := int32(0)
	if err != nil {
		code = 1
		_ = sink.Send(&pb.TranscodeLog{StderrChunk: []byte("remux: " + err.Error() + "\n")})
	}
	_ = sink.Send(&pb.TranscodeLog{ExitCode: code, IsFinished: true})
	return err
}

func writeFile(dir, name string, data []byte) error {
	if !segmentName.MatchString(name) {
		return fmt.Errorf("remux: refusing file name %q", name)
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// send makes a request to Plex at its own address: the job's URL names
// 127.0.0.1:32400, which from the manager's namespace is the proxy.
// sub is appended to the path ("stream", "streamDetail"); a non-empty query
// replaces the URL's own.
func (r *Relay) send(ctx context.Context, method, raw, sub, query string, body []byte, token string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	base, err := url.Parse(r.PMS)
	if err != nil {
		return err
	}
	u.Scheme, u.Host = base.Scheme, base.Host
	if sub != "" {
		u.Path += "/" + sub
	}
	if query != "" {
		u.RawQuery = query
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, u.Path, resp.Status)
	}
	return nil
}
```


- [ ] **Step 4: Run the relay tests**

Run: `go test ./pkg/remux/relay/`
Expected: PASS.

- [ ] **Step 5: Hook the dispatcher (test first)** — append to `pkg/remoteexec/dispatcher_test.go`:

```go
type remuxRoute struct {
	handled bool
	calls   int
}

func (r *remuxRoute) Execute(context.Context, *pb.ExecRequest, Sink) (bool, error) {
	r.calls++
	return r.handled, nil
}

func TestABrowserRemuxNeverReachesPlexsTranscoder(t *testing.T) {
	route := &remuxRoute{handled: true}
	d := &Dispatcher{Remux: route}
	require.NoError(t, d.Execute(t.Context(), &pb.ExecRequest{TargetBinary: Transcoder}, nil))
	assert.Equal(t, 1, route.calls)
}
```
Run: `go test -run TestABrowserRemux ./pkg/remoteexec/` — Expected: FAIL to build: `unknown field Remux`.

In `pkg/remoteexec/dispatcher.go` add:
```go
// RemuxRoute takes a browser remux off Plex's transcoder (pkg/remux/relay).
type RemuxRoute interface {
	Execute(ctx context.Context, req *pb.ExecRequest, sink Sink) (done bool, err error)
}
```
add the field `Remux RemuxRoute // nil leaves every job to Plex's helpers` to `Dispatcher`, and at the top of `Execute`:
```go
	if d.Remux != nil && req.GetTargetBinary() == Transcoder {
		if done, err := d.Remux.Execute(ctx, req, sink); done {
			d.route(req.GetTargetBinary(), "remux")
			return err
		}
	}
```
Run: `go test ./pkg/remoteexec/` — Expected: PASS.

- [ ] **Step 6: Config and wiring** — in `cmd/manager/config.go` add to `Config`:
```go
	// RemuxSelector picks the remux pool's pods; empty turns the pool off.
	RemuxSelector string
	// RemuxPort is the remux workers' gRPC port.
	RemuxPort int
```
in `newFlagSet`:
```go
	fs.String("remux-selector", "app=plex-remux", "label selector of the remux pool's pods; empty leaves every browser stream to Plex's transcoder")
	fs.Int("remux-port", 50052, "gRPC port of the remux workers")
```
and in the config builder: `RemuxSelector: strings.TrimSpace(v.GetString("remux-selector")), RemuxPort: port("remux-port"),`. Add to `cmd/manager/config_test.go` a test that the defaults are `app=plex-remux` and `50052` (write it first, watch it fail).

In `cmd/manager/execserver.go`, after building `dispatcher`:
```go
	if cfg.RemuxSelector != "" {
		dispatcher.Remux = &relay.Relay{
			Players: m.pms(),
			Workers: &remoteexec.PodWorkerLister{Client: m.K8sClient, Namespace: cfg.Namespace, Self: cfg.PodName,
				Selector: cfg.RemuxSelector, Port: cfg.RemuxPort},
			Dial:   relay.GRPCDialer,
			PMS:    "http://" + m.plexAddr,
			Logger: m.Logger.With("component", "remux"),
		}
	}
```
and in `relay.go`:
```go
// GRPCDialer dials a remux worker over plaintext gRPC inside the cluster.
func GRPCDialer(_ context.Context, addr string) (remuxpb.RemuxClient, io.Closer, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return remuxpb.NewRemuxClient(conn), conn, nil
}
```

- [ ] **Step 7: The no-ffgo guard** — `cmd/manager/noffgo_test.go`:

```go
package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The manager, shim, proxy and maintenance binaries are static and run in
// a scratch image; ffgo (purego) would make them dynamic. Only
// cmd/remux-worker may link it.
func TestNoStaticBinaryLinksFFgo(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "../manager", "../shim", "../proxy", "../maintenance").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(dep, "ffgo") || strings.Contains(dep, "purego") {
			t.Fatalf("a static binary links %s", dep)
		}
	}
}
```
Falsify: temporarily add `_ "github.com/mediactl/clusterplex/pkg/remux/pipeline"` to `cmd/manager/execserver.go`, run the test, see it FAIL naming ffgo; remove it.

- [ ] **Step 8: Run everything**

Run: `make test && make lint`
Expected: PASS, 0 issues.

- [ ] **Step 9: Commit**

```bash
git commit -m "feat(manager): browser remuxes go to the remux pool -- classified, hashed to a worker, segments written into the session directory before Plex hears of them, progress and manifest relayed with the session's token; no static binary links ffgo" -- pkg/remux/relay pkg/remoteexec/dispatcher.go pkg/remoteexec/dispatcher_test.go cmd/manager/config.go cmd/manager/config_test.go cmd/manager/execserver.go cmd/manager/noffgo_test.go
```

---

### Task 11: the image and the pool's manifests

**Files:**
- Create: `images/Dockerfile.remux`, `images/stage.sh` (copied from clustarr's `images/distroless/stage.sh`; same owner)
- Create: `k8s/base/remux.yaml`; modify `k8s/base/kustomization.yaml`
- Modify: `k8s/overlays/kind-cluster-plex/kustomization.yaml` (image tag, `/library` mount on the remux Deployment)
- Create: `charts/cluster-plex/templates/remux.yaml`; modify `charts/cluster-plex/values.yaml`
- Modify: `Makefile` (`docker-build-remux`, `kind-load-remux`)

- [ ] **Step 1: The Dockerfile** — `images/Dockerfile.remux`: clustarr's `images/Dockerfile.transcoder` `build` and `ffmpeg` stages verbatim, except that `build` builds `-o /out/remux-worker ./cmd/remux-worker` with `-X` dropped, and copies cluster-plex's (no) licence lines removed; then:

```dockerfile
FROM debian:trixie-slim AS staging
RUN apt-get update \
 && apt-get install -y --no-install-recommends pax-utils ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=ffmpeg /out/ /
COPY --from=build /out/remux-worker /usr/bin/remux-worker
COPY --from=build /out/licenses/ /usr/share/licenses/
COPY images/stage.sh /stage.sh
RUN /stage.sh /staging

FROM scratch AS remux
LABEL org.opencontainers.image.source="https://github.com/mediactl/cluster-plex" \
      org.opencontainers.image.title="cluster-plex-remux"
COPY --from=staging /staging/ /
ENV FFGO_SHIM_DIR=/usr/lib
USER 1000:1000
ENTRYPOINT ["/usr/bin/remux-worker"]
```
No Intel or NVIDIA stage: the pool decodes and encodes audio only.

Run: `cp /home/appkins/src/mediactl/clustarr/images/distroless/stage.sh images/stage.sh && docker build -f images/Dockerfile.remux --target remux -t ghcr.io/mediactl/cluster-plex-remux:dev .`
Expected: the image builds (requires the ffgo tag pushed and `go.mod`'s replace in its pushed form, Task 2 Step 1).

Run: `docker run --rm ghcr.io/mediactl/cluster-plex-remux:dev --help`
Expected: the flag list, no loader error.

- [ ] **Step 2: The Deployment** — `k8s/base/remux.yaml`:

```yaml
# The remux pool: Plex Web's DASH streams, remuxed in process with ffgo
# (docs/superpowers/specs/2026-10-01-browser-remux-transcoder-design.md).
# Managers find these pods by app=plex-remux and dial :50052; the cache is
# per pod, and the managers' hash ring sends a file back to the pod holding it.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: plex-remux
spec:
  replicas: 2
  selector:
    matchLabels: {app: plex-remux}
  template:
    metadata:
      labels: {app: plex-remux}
    spec:
      securityContext: {runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000, runAsNonRoot: true}
      containers:
        - name: remux
          image: ghcr.io/mediactl/cluster-plex-remux:dev
          args: ["--cache-dir=/cache", "--cache-max-bytes=21474836480"]
          ports:
            - {name: grpc, containerPort: 50052}
            - {name: probe, containerPort: 8080}
          readinessProbe: {httpGet: {path: /readyz, port: probe}}
          livenessProbe: {httpGet: {path: /healthz, port: probe}}
          resources:
            requests: {cpu: 250m, memory: 256Mi}
            limits: {memory: 1Gi}
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
          volumeMounts:
            - {name: cache, mountPath: /cache}
            - {name: media, mountPath: /media, readOnly: true}
      volumes:
        - name: cache
          emptyDir: {sizeLimit: 25Gi}
        - name: media
          persistentVolumeClaim: {claimName: plex-media, readOnly: true}
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: plex-remux
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: plex-remux}
  minReplicas: 1
  maxReplicas: 6
  metrics:
    - type: Resource
      resource: {name: cpu, target: {type: Utilization, averageUtilization: 70}}
```
Match the `media` volume's claim and mount path to `k8s/base/statefulset.yaml`'s media volume (read it; use the same claim name and mount path). Add `- remux.yaml` to `k8s/base/kustomization.yaml`'s resources.

In `k8s/overlays/kind-cluster-plex/kustomization.yaml` add under `images:`:
```yaml
  - name: ghcr.io/mediactl/cluster-plex-remux
    newTag: "<the commit's short SHA>"
```
and a patch giving the remux Deployment the same `/library` mount the Plex StatefulSet gets:
```yaml
  - target:
      kind: Deployment
      name: plex-remux
    patch: |-
      - op: add
        path: /spec/template/spec/volumes/-
        value:
          name: clustarr-library
          persistentVolumeClaim: {claimName: plex-clustarr-library, readOnly: true}
      - op: add
        path: /spec/template/spec/containers/0/volumeMounts/-
        value: {name: clustarr-library, mountPath: /library, subPath: media, readOnly: true}
```
Run: `kubectl kustomize k8s/overlays/kind-cluster-plex | grep -A3 'name: plex-remux' | head` — Expected: the Deployment and HPA render.

- [ ] **Step 3: The chart** — `charts/cluster-plex/templates/remux.yaml` renders the same Deployment and HPA from `values.yaml`:
```yaml
remux:
  enabled: true
  image: {repository: ghcr.io/mediactl/cluster-plex-remux, tag: ""}
  replicas: 2
  autoscaling: {minReplicas: 1, maxReplicas: 6, targetCPU: 70}
  cache: {sizeLimit: 25Gi, maxBytes: 21474836480}
  resources: {requests: {cpu: 250m, memory: 256Mi}, limits: {memory: 1Gi}}
```
and passes `--remux-selector=` (empty) to the manager when `remux.enabled` is false. Run: `make helm-lint` — Expected: PASS.

- [ ] **Step 4: Makefile** — add:
```make
REMUX_IMG ?= ghcr.io/mediactl/cluster-plex-remux:dev
.PHONY: docker-build-remux
docker-build-remux: ## Build the remux worker's image
	docker build -f images/Dockerfile.remux --target remux -t $(REMUX_IMG) .
```

- [ ] **Step 5: Commit**

```bash
git add images/Dockerfile.remux images/stage.sh k8s/base/remux.yaml charts/cluster-plex/templates/remux.yaml
git commit -m "deploy: the remux pool -- an FFmpeg 9 scratch image with the worker, a Deployment and HPA with a per-pod cache, the kind overlay's /library mount" -- Makefile images/Dockerfile.remux images/stage.sh k8s/base/remux.yaml k8s/base/kustomization.yaml k8s/overlays/kind-cluster-plex/kustomization.yaml charts/cluster-plex/templates/remux.yaml charts/cluster-plex/values.yaml
```

---

### Task 12: end to end, and the record

**Files:**
- Create: `test/e2e/remux_test.go` (build tag `e2e`)
- Create: `docs/adr/0007-browser-streams-on-the-remux-pool.md`
- Modify: `CLAUDE.md` (layout table, invariants), `docs/configuration.md` (the two flags)

- [ ] **Step 1: The e2e test** — `test/e2e/remux_test.go` asks Plex, as Plex Web, for a DASH stream of the first movie, fetches the manifest and the first segments, and checks a remux pod served it; then asks again and checks the second answer came from the cache (the worker logs `remux served from cache` — add that `Info` log line to `worker.Server.remux` when a chain is served, in this task, with its unit assertion in `server_test.go`):

```go
//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPlexWebPlaysThroughTheRemuxPool(t *testing.T) {
	plex, token := plexEndpoint(t) // the suite's existing helper for the proxy address and the admin token
	item := firstMovieKey(t, plex, token)
	start := func(session string) *http.Response {
		q := url.Values{
			"path": {item}, "protocol": {"dash"}, "directPlay": {"0"}, "directStream": {"1"},
			"mediaIndex": {"0"}, "partIndex": {"0"}, "session": {session},
			"X-Plex-Client-Identifier": {"e2e-" + session}, "X-Plex-Product": {"Plex Web"},
			"X-Plex-Platform": {"Chrome"}, "X-Plex-Token": {token},
		}
		resp, err := http.Get(plex + "/video/:/transcode/universal/start.mpd?" + q.Encode())
		require.NoError(t, err)
		return resp
	}
	for i, session := range []string{"remux-a", "remux-b"} {
		resp := start(session)
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
		require.Contains(t, string(b), "chunk-stream0-")
		seg, err := http.Get(fmt.Sprintf("%s/video/:/transcode/universal/session/%s/0/1.m4s?X-Plex-Token=%s", plex, session, token))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, seg.StatusCode)
		_ = seg.Body.Close()
		want := "remux"
		if i == 1 {
			want = "served from cache"
		}
		require.Eventually(t, func() bool { return strings.Contains(remuxLogs(t), want) }, time.Minute, time.Second)
	}
}
```
Add beside the suite's existing helpers (the suite already has `plexEndpoint`; if its name differs, use the suite's proxy-address and token helpers):
```go
// firstMovieKey is the metadata key of the first item in Clustarr Movies.
func firstMovieKey(t *testing.T, plex, token string) string {
	t.Helper()
	var out struct {
		MediaContainer struct {
			Directory []struct{ Key, Title string } `json:"Directory"`
			Metadata  []struct{ Key string }        `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	get := func(path string) {
		req, _ := http.NewRequest(http.MethodGet, plex+path, nil)
		req.Header.Set("X-Plex-Token", token)
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	}
	get("/library/sections")
	section := ""
	for _, d := range out.MediaContainer.Directory {
		if d.Title == "Clustarr Movies" {
			section = d.Key
		}
	}
	require.NotEmpty(t, section, "no Clustarr Movies library")
	get("/library/sections/" + section + "/all")
	require.NotEmpty(t, out.MediaContainer.Metadata, "Clustarr Movies is empty")
	return out.MediaContainer.Metadata[0].Key
}

// remuxLogs is the remux pool's recent log.
func remuxLogs(t *testing.T) string {
	t.Helper()
	out, _ := exec.Command("kubectl", "--context", "kind-cluster-plex", "-n", "media", "logs", "-l", "app=plex-remux", "--tail=200").CombinedOutput()
	return string(out)
}
```
with `"encoding/json"` and `"os/exec"` added to the imports. The segment URL is Plex Web's form; if the first run's manifest names segments differently, follow the manifest's template for the path and keep the assertions.

Run (with the owner's go-ahead to deploy to kind-cluster-plex): `make e2e`
Expected: PASS. Note: this deploys; the live cluster's overlay is `k8s/overlays/kind-cluster-plex` (never `make deploy-kind`).

- [ ] **Step 2: ADR 0007** — `docs/adr/0007-browser-streams-on-the-remux-pool.md`: Status accepted (2026-10-01). Context: with `TranscoderCanOnlyRemuxVideo`, Plex's only transcode is Plex Web's MKV→DASH remux with an audio downmix; remote dispatch could never serve it (the session directory is per pod). Decision: the remux pool (ffgo in process, segments streamed back, the manager writes them before posting the manifest; progress relayed; a per-worker cache keyed by file and audio choice, hashed to its worker). Consequences: Plex pods carry no FFmpeg 9; anything the parser refuses stays Plex's; browsers without HEVC decode still need a real-time transcode.

- [ ] **Step 3: CLAUDE.md and configuration.md** — add `pkg/remux/` (classify, parse, manifest, splitter, cache, pipeline, worker, relay) and `cmd/remux-worker/` to the layout table; add the invariant "Only cmd/remux-worker links ffgo (`TestNoStaticBinaryLinksFFgo`)"; document `--remux-selector` and `--remux-port` in `docs/configuration.md`'s flag list.

- [ ] **Step 4: Commit**

```bash
git commit -m "docs: ADR 0007 browser streams on the remux pool; the remux packages in CLAUDE.md; e2e playback through the pool and its cache" -- test/e2e/remux_test.go docs/adr/0007-browser-streams-on-the-remux-pool.md CLAUDE.md docs/configuration.md pkg/remux/worker/server.go pkg/remux/worker/server_test.go
```
