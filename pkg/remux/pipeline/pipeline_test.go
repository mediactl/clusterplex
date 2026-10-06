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
	return remux.Job{
		Input: input, SkipToSegment: skip, Start: time.Duration(skip-1) * 5 * time.Second,
		SegmentDuration: 5 * time.Second, VideoStream: 0, AudioStream: 1,
		AudioChannels: 2, AudioSampleRate: 48000, AudioBitRate: 256000, AudioLanguage: "eng",
	}
}

type recorder struct {
	init     [2][]byte
	scales   [2]int32
	info     remux.StreamInfo
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
	// Review finding I6: only a segment that starts where a seek to it
	// would start it may answer that seek from the cache. The full run's
	// segment 3 (20 s) is not; the seek's own (10 s) is.
	var grid []bool
	for _, s := range r.segments {
		grid = append(grid, s.OnGrid)
	}
	assert.Equal(t, []bool{true, true, false}, grid)
	assert.True(t, seek.segments[0].OnGrid)
}

func TestASeekRunsVideoIsTheFullRunsVideo(t *testing.T) {
	in := clip(t, 20, 48)
	full := run(t, job(in, 1), Options{From: 1, StartAt: -1})
	seek := run(t, job(in, 3), Options{From: 3, StartAt: -1})
	require.Equal(t, 3, seek.segments[0].N)
	assert.Equal(t, full.segments[2].Start, seek.segments[0].Start)
	// The same frames, spaced the same, in source time. Not the same bytes
	// or exactly the same times: Matroska stores no DTS, libavformat derives
	// it from the B-frame delay, and after a seek that derivation restarts
	// a constant reorder delay (here 3 frames) off a run from the start --
	// as it does for Plex's own transcoder on every seek.
	want := framePTS(t, full.init[0], full.segments[2].Video.Data)
	got := framePTS(t, seek.init[0], seek.segments[0].Video.Data)
	require.NotEmpty(t, want)
	require.Len(t, got, len(want), "the same frames")
	offset := got[0] - want[0]
	ms := int64(seek.scales[0]) / 1000 // Matroska's timestamps are whole milliseconds
	for i := range want {
		assert.InDelta(t, offset, got[i]-want[i], float64(ms), "frame %d keeps the same spacing", i)
	}
	assert.LessOrEqual(t, abs(offset), int64(seek.scales[0])/4, "within a quarter second: placed in source time, not at 0")
	assert.Equal(t, full.init[0], seek.init[0])
}

// Review focus 2: every audio fragment ends at its segment's video
// boundary, within one AAC frame.
func TestAudioFragmentsEndAtTheVideoBoundaries(t *testing.T) {
	r := run(t, job(clip(t, 20, 48), 1), Options{From: 1, StartAt: -1})
	frame := time.Duration(1024) * time.Second / 48000
	require.Len(t, r.segments, 4, "every segment pairs its audio with its video")
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

// framePTS is the presentation time of every frame ffprobe decodes from
// one fragment behind its init.
func framePTS(t *testing.T, init, frag []byte) []int64 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "frag.mp4")
	require.NoError(t, os.WriteFile(path, append(bytes.Clone(init), frag...), 0o644))
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v", "-show_entries", "frame=pts", "-of", "csv=p=0", path).Output()
	require.NoError(t, err)
	var pts []int64
	for _, f := range strings.Fields(string(out)) {
		v, err := strconv.ParseInt(strings.TrimSuffix(f, ","), 10, 64)
		require.NoError(t, err)
		pts = append(pts, v)
	}
	return pts
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Review finding C1: a run that fills a cache gap starts at o.From, past the
// job's own first segment, and its audio must be numbered from there too.
func TestAGapFillPairsEachSegmentWithItsOwnAudio(t *testing.T) {
	in := clip(t, 20, 48)
	full := run(t, job(in, 1), Options{From: 1, StartAt: -1})
	j := job(in, 1) // the job asked for segment 1; segment 1 came from the cache
	gap := run(t, j, Options{From: 2, StartAt: full.segments[0].End})
	require.Len(t, gap.segments, len(full.segments)-1, "every remaining segment, the last included")
	for i, s := range gap.segments {
		want := full.segments[i+1]
		assert.Equal(t, want.N, s.N)
		assert.Equal(t, want.Start, s.Start)
		start := time.Duration(s.Audio.T) * time.Second / time.Duration(gap.scales[1])
		assert.InDelta(t, float64(s.Start), float64(start), float64(100*time.Millisecond), "segment %d's audio starts with its video", s.N)
	}
}
