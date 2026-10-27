package pipeline_test

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/pipeline"
)

// liveTS is s seconds of a Live TV-like MPEG-TS: H.264 720p at 29.97 fps,
// a keyframe every 2 s, and AAC stereo, its timestamps starting at offset.
func liveTS(t *testing.T, s, offset int) []byte {
	t.Helper()
	out := makeClip(t, filepath.Join(t.TempDir(), "live.ts"),
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30000/1001",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", strconv.Itoa(s), "-c:v", "libx264", "-preset", "ultrafast", "-g", "60", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k", "-output_ts_offset", strconv.Itoa(offset), "-f", "mpegts")
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	return b
}

// liveJob is library-hls on a streamed MPEG-TS of data.
func liveJob(t *testing.T, data []byte) (video.Job, pipeline.Options) {
	t.Helper()
	job := library(t, "")
	job.Inputs[0] = video.Input{Streamed: true, Format: "mpegts"}
	o := cpu
	o.Inputs = func(int) io.Reader { return bytes.NewReader(data) }
	return job, o
}

// covered is the media time a run's segments cover, and where the last ends.
func covered(r *recorder) (sum, end time.Duration) {
	for _, s := range r.segments {
		sum += s.End - s.Start
		end = s.End
	}
	return sum, end
}

// oracle is ffmpeg(1) converting the same bytes: it must finish.
func oracle(t *testing.T, data []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.ts")
	require.NoError(t, os.WriteFile(path, data, 0o644))
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-c:v", "libx264", "-preset", "ultrafast",
		"-c:a", "aac", "-f", "null", "-").CombinedOutput()
	require.NoError(t, err, "ffmpeg(1) carries on: %s", out)
}

func TestADamagedLiveStreamCarriesOn(t *testing.T) {
	// IPTV after a network hiccup: every 37th TS packet of the middle third
	// garbled past its header. ffmpeg(1) skips what it cannot decode; one
	// bad packet must not end the stream.
	need(t)
	data := liveTS(t, 16, 0)
	rng := rand.New(rand.NewSource(1))
	n := len(data) / 188
	for p := n / 3; p < 2*n/3; p += 37 {
		for i := 8; i < 188; i++ {
			data[p*188+i] = byte(rng.Intn(256))
		}
	}
	oracle(t, data)
	job, o := liveJob(t, data)
	var skipped atomic.Int32
	o.Skipped = func(error) { skipped.Add(1) }
	r := &recorder{}
	require.NoError(t, pipeline.Run(t.Context(), job, o, r))
	sum, _ := covered(r)
	assert.InDelta(t, 16, sum.Seconds(), 1, "the whole stream is converted")
	assert.Positive(t, skipped.Load(), "what the decoders refused is counted")
}

func TestATimestampJumpKeepsTheTimeline(t *testing.T) {
	// An IPTV upstream's encoder restarts: its timestamps jump. ffmpeg(1)
	// re-anchors an MPEG-TS on a jump; a jump forward must not become a
	// frozen picture, nor one back (which libavformat reads as a 33-bit
	// wrap, 26.5 h on) millions of copies of one frame.
	need(t)
	for name, at := range map[string][2]int{"forward": {100, 160}, "back": {1000, 100}} {
		t.Run(name, func(t *testing.T) {
			data := append(liveTS(t, 12, at[0]), liveTS(t, 12, at[1])...)
			oracle(t, data)
			job, o := liveJob(t, data)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			r := &recorder{}
			require.NoError(t, pipeline.Run(ctx, job, o, r))
			sum, end := covered(r)
			assert.InDelta(t, 24, sum.Seconds(), 1.5, "24 s in, 24 s out")
			assert.InDelta(t, 24, end.Seconds(), 1.5, "and continuous")
		})
	}
}

func TestALiveInputReportsNoProgressKnown(t *testing.T) {
	// Plex's transcoder reports a live input's progress as -1.0 and its
	// remaining time as -1 (the capture's trace); 0.0 and 0, a job at its
	// start with nothing left, kept Plex from answering a browser's
	// start.mpd for 28 s (talos, rev 116).
	need(t)
	job, o := liveJob(t, liveTS(t, 4, 0))
	r := &recorder{}
	require.NoError(t, pipeline.Run(t.Context(), job, o, r))
	require.GreaterOrEqual(t, len(r.progress), 3)
	// First, as Plex's transcoder: the input's duration, unknown, and its
	// picture's size. Plex holds a live session's start until it has the
	// duration ("timed out waiting to find duration for live session").
	assert.Equal(t, "?duration=-1.000000", r.progress[0])
	assert.Equal(t, "?width=1280&height=720", r.progress[1])
	for _, p := range r.progress[2:] {
		assert.Regexp(t, `^\?progress=-1\.0&size=-1&remaining=-1&speed=\d+\.\d&vdec_hw_status=\d$`, p)
	}
}
