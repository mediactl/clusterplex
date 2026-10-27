package pipeline_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/pipeline"
)

// skipOrFail skips a test that needs FFmpeg 9 and its CLI for clips,
// unless make test-native asked for the ffgo suites to run for real.
func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CLUSTERPLEX_REQUIRE_FFGO") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

func need(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			skipOrFail(t, "no %s to make or inspect clips", bin)
		}
	}
	if err := ffgo.Init(); err != nil || ffgo.ShimAPIVersion() < 2 {
		skipOrFail(t, "FFmpeg 9 with shim API 2 not available: %v", err)
	}
}

// clip is s seconds of HEVC 1280x720 at 25 fps with a keyframe every 250
// frames (x265's default, much of the library) and AAC 5.1 at 48 kHz, in
// the container ext names.
func clip(t *testing.T, s int, ext string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "clip."+ext)
	cmd := exec.Command("ffmpeg", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000,aformat=channel_layouts=5.1",
		"-t", strconv.Itoa(s), "-c:v", "libx265", "-preset", "ultrafast",
		"-x265-params", "log-level=none:keyint=250:min-keyint=250:scenecut=0",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "384k", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make clip: %v %s", err, b)
	}
	return out
}

// library is the captured library HLS job (pkg/video's fixture) on input.
func library(t *testing.T, input string) video.Job {
	t.Helper()
	f, err := os.Open("../testdata/plex-video-jobs.jsonl")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		var c struct {
			Shape string            `json:"shape"`
			Args  []string          `json:"args"`
			Env   map[string]string `json:"env"`
		}
		require.NoError(t, json.Unmarshal(s.Bytes(), &c))
		if c.Shape != "library-hls" {
			continue
		}
		j, err := video.Parse(c.Args, c.Env)
		require.NoError(t, err)
		j.Inputs[0].Path = input
		j.Audio.Stream = 1 // the clip's audio
		return video.JobFromProto(j.Proto())
	}
	t.Fatal("no library-hls fixture")
	return video.Job{}
}

// recorder keeps what a run sends.
type recorder struct {
	mu       sync.Mutex
	segments []pipeline.Segment
	progress []string
	onSeg    func(pipeline.Segment)
}

func (r *recorder) Segment(s pipeline.Segment) error {
	r.mu.Lock()
	r.segments = append(r.segments, s)
	on := r.onSeg
	r.mu.Unlock()
	if on != nil {
		on(s)
	}
	return nil
}
func (r *recorder) File(string, []byte) error { return errors.New("no files in an HLS job") }
func (r *recorder) Manifest([]byte) error     { return errors.New("no manifest in an HLS job") }

func (r *recorder) Write(int64, []byte) error { return errors.New("no writes in an HLS job") }

func (r *recorder) Progress(path, query string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.progress = append(r.progress, path+"?"+query)
}

var cpu = pipeline.Options{Encoder: "libx264"}

// probe is ffprobe's answer about a file's streams and frames.
type probe struct {
	Streams []struct {
		CodecName  string `json:"codec_name"`
		CodecType  string `json:"codec_type"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		PixFmt     string `json:"pix_fmt"`
		AvgRate    string `json:"avg_frame_rate"`
		SampleRate string `json:"sample_rate"`
		Channels   int    `json:"channels"`
		BitRate    string `json:"bit_rate"`
	} `json:"streams"`
	Frames []struct {
		MediaType string `json:"media_type"`
		KeyFrame  int    `json:"key_frame"`
		PTSTime   string `json:"pts_time"`
	} `json:"frames"`
}

func ffprobe(t *testing.T, data []byte) probe {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seg.ts")
	require.NoError(t, os.WriteFile(path, data, 0o644))
	out, err := exec.Command("ffprobe", "-v", "error", "-show_streams", "-show_frames",
		"-show_entries", "frame=media_type,key_frame,pts_time", "-of", "json", path).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		t.Fatalf("ffprobe: %v: %s", err, ee.Stderr)
	}
	require.NoError(t, err)
	var p probe
	require.NoError(t, json.Unmarshal(out, &p))
	return p
}

func videoFrames(p probe) (n int, firstKey bool, first float64) {
	for _, f := range p.Frames {
		if f.MediaType != "video" {
			continue
		}
		if n == 0 {
			firstKey = f.KeyFrame == 1
			first, _ = strconv.ParseFloat(f.PTSTime, 64)
		}
		n++
	}
	return n, firstKey, first
}

func TestSegmentsStartOnKeyframesEverySegmentTime(t *testing.T) {
	need(t)
	job := library(t, clip(t, 40, "mkv"))
	r := &recorder{}
	require.NoError(t, pipeline.Run(t.Context(), job, cpu, r))

	require.Len(t, r.segments, 5, "four 8.008 s segments and the rest")
	for i, s := range r.segments {
		assert.Equal(t, video.SegmentName("media-%05d.ts", i), s.Name)
		n, key, _ := videoFrames(ffprobe(t, s.Data))
		assert.True(t, key, "segment %d starts on a keyframe", i)
		if i < len(r.segments)-1 {
			assert.InDelta(t, 192, n, 1, "segment %d is 8.008 s at 24000/1001", i)
			require.NotNil(t, s.NextStart, "segment %d names where the next starts", i)
		} else {
			assert.Nil(t, s.NextStart, "the last segment has no next")
		}
	}
}

func TestARestartNumbersFromItsStartNumber(t *testing.T) {
	need(t)
	job := library(t, clip(t, 30, "mkv"))
	job.Start, job.Segments.StartNumber = 16*time.Second, 2
	r := &recorder{}
	require.NoError(t, pipeline.Run(t.Context(), job, cpu, r))
	require.NotEmpty(t, r.segments)
	assert.Equal(t, "media-00002.ts", r.segments[0].Name)
	assert.InDelta(t, 16.0, r.segments[0].Start.Seconds(), 0.042, "the first segment starts at -ss, within a frame")
	_, key, first := videoFrames(ffprobe(t, r.segments[0].Data))
	assert.True(t, key)
	assert.InDelta(t, 16.0, first, 0.042, "timestamps stay in source time (-copyts)")
}

func TestTheOutputIsWhatPlexAskedFor(t *testing.T) {
	need(t)
	job := library(t, clip(t, 20, "mkv"))
	r := &recorder{}
	require.NoError(t, pipeline.Run(t.Context(), job, cpu, r))
	var all []byte
	for _, s := range r.segments {
		all = append(all, s.Data...)
	}
	p := ffprobe(t, all)
	var v, a bool
	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			v = true
			assert.Equal(t, "h264", s.CodecName)
			assert.Equal(t, 720, s.Width)
			assert.Equal(t, 404, s.Height)
			assert.Equal(t, "yuv420p", s.PixFmt)
			assert.Equal(t, "24000/1001", s.AvgRate)
		case "audio":
			a = true
			assert.Equal(t, "aac", s.CodecName)
			assert.Equal(t, 2, s.Channels)
			assert.Equal(t, "96000", s.SampleRate)
		}
	}
	assert.True(t, v && a, "a video and an audio stream")
	n, _, _ := videoFrames(p)
	assert.InDelta(t, 20*24000.0/1001, n, 2, "constant frame rate at Plex's rate from a 25 fps source")
}

func TestTheBitrateStaysUnderMaxrate(t *testing.T) {
	need(t)
	job := library(t, clip(t, 24, "mkv"))
	r := &recorder{}
	require.NoError(t, pipeline.Run(t.Context(), job, cpu, r))
	var all []byte
	for _, s := range r.segments {
		all = append(all, s.Data...)
	}
	path := filepath.Join(t.TempDir(), "all.ts")
	require.NoError(t, os.WriteFile(path, all, 0o644))
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries",
		"packet=pts_time,size", "-of", "csv=p=0", path).Output()
	require.NoError(t, err)
	type pkt struct {
		t    float64
		bits float64
	}
	var ps []pkt
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ",")
		tt, _ := strconv.ParseFloat(f[0], 64)
		sz, _ := strconv.ParseFloat(f[1], 64)
		ps = append(ps, pkt{tt, sz * 8})
	}
	window := 2 * float64(job.Video.BufSize) / float64(job.Video.MaxRate) // seconds
	limit := 1.1 * float64(job.Video.MaxRate) * window
	for i := range ps {
		sum := 0.0
		for j := i; j < len(ps) && ps[j].t < ps[i].t+window; j++ {
			sum += ps[j].bits
		}
		require.LessOrEqual(t, sum, limit, "%.0f bits in the %.1f s from %.3f s", sum, window, ps[i].t)
	}
}

func TestTheSegmentTimesAreThoseItsLineReports(t *testing.T) {
	need(t)
	job := library(t, clip(t, 20, "mkv"))
	r := &recorder{}
	require.NoError(t, pipeline.Run(t.Context(), job, cpu, r))
	for i, s := range r.segments {
		_, _, first := videoFrames(ffprobe(t, s.Data))
		assert.InDelta(t, s.Start.Seconds(), first, 0.002, "segment %d starts where its line says", i)
		if i+1 < len(r.segments) {
			assert.Equal(t, r.segments[i+1].Start, s.End, "segment %d ends where the next starts", i)
			assert.Equal(t, s.End, *s.NextStart)
		}
	}
}

func TestAStreamedInputConverts(t *testing.T) {
	need(t)
	src := clip(t, 12, "ts")
	job := library(t, "")
	job.Inputs[0] = video.Input{Streamed: true, Format: "mpegts"}
	o := cpu
	o.Inputs = func(int) io.Reader {
		f, err := os.Open(src)
		require.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	r := &recorder{}
	require.NoError(t, pipeline.Run(t.Context(), job, o, r))
	require.Len(t, r.segments, 2)
}

func TestACancelledConversionStops(t *testing.T) {
	need(t)
	job := library(t, clip(t, 60, "mkv"))
	ctx, cancel := context.WithCancel(t.Context())
	r := &recorder{onSeg: func(pipeline.Segment) { cancel() }}
	began := time.Now()
	err := pipeline.Run(ctx, job, cpu, r)
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(began), 30*time.Second)
	assert.Len(t, r.segments, 1, "nothing past the cancel")
}

func TestProgressNamesPlexsKeys(t *testing.T) {
	need(t)
	job := library(t, clip(t, 12, "mkv"))
	r := &recorder{}
	require.NoError(t, pipeline.Run(t.Context(), job, cpu, r))
	require.NotEmpty(t, r.progress)
	for _, p := range r.progress {
		path, query, _ := strings.Cut(p, "?")
		if path != "" {
			continue
		}
		if strings.HasPrefix(query, "progress=") {
			assert.Regexp(t, `^progress=\d+\.\d&size=-1&remaining=\d+&speed=\d+\.\d&vdec_hw_status=0$`, query)
		}
	}
}

func TestAJobItCannotDrawYetIsRefused(t *testing.T) {
	// Until burn-in is built (Task 12), such a job is refused as a shape,
	// never converted without it.
	for name, edit := range map[string]func(*video.Job){
		"a burn-in": func(j *video.Job) { j.Burn = &video.BurnIn{Stream: 3, Image: true} },
	} {
		t.Run(name, func(t *testing.T) {
			job := library(t, "/media/none.mkv")
			edit(&job)
			err := pipeline.Run(t.Context(), job, cpu, &recorder{})
			require.ErrorIs(t, err, video.ErrShape)
		})
	}
}
