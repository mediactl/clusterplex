package pipeline_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/pipeline"
)

// makeClip runs ffmpeg to make a test clip at out.
func makeClip(t *testing.T, out string, args ...string) string {
	t.Helper()
	cmd := exec.Command("ffmpeg", append(append([]string{"-v", "error", "-y"}, args...), out)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make clip: %v %s", err, b)
	}
	return out
}

// convert runs job on o and returns its segments joined into one file.
func convert(t *testing.T, job video.Job, o pipeline.Options) string {
	t.Helper()
	r := &recorder{}
	require.NoError(t, pipeline.Run(t.Context(), job, o, r))
	require.NotEmpty(t, r.segments)
	var all bytes.Buffer
	for _, s := range r.segments {
		all.Write(s.Data)
	}
	out := filepath.Join(t.TempDir(), "out.ts")
	require.NoError(t, os.WriteFile(out, all.Bytes(), 0o644))
	return out
}

var idetLine = regexp.MustCompile(`Multi frame detection: TFF:\s*(\d+) BFF:\s*(\d+) Progressive:\s*(\d+)`)

// idet is FFmpeg's interlace detector's verdict on path's frames: how many
// it read as interlaced (either field first) and as progressive.
func idet(t *testing.T, path string) (interlaced, progressive int) {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-v", "info", "-nostats", "-i", path, "-vf", "idet", "-an", "-f", "null", "-").CombinedOutput()
	require.NoError(t, err, "%s", out)
	// FFmpeg prints the summary once for a first, empty graph too: the last
	// one counts.
	all := idetLine.FindAllSubmatch(out, -1)
	require.NotEmpty(t, all, "%s", out)
	m := all[len(all)-1]
	tff, _ := strconv.Atoi(string(m[1]))
	bff, _ := strconv.Atoi(string(m[2]))
	prog, _ := strconv.Atoi(string(m[3]))
	return tff + bff, prog
}

// interlacedClip is 4 s of 720x480 MPEG-2 at 29.97 frames a second, top
// field first, each frame woven from two moments of motion (a 1080i or
// SD Live TV channel's shape), in MPEG-TS.
func interlacedClip(t *testing.T) string {
	t.Helper()
	return makeClip(t, filepath.Join(t.TempDir(), "interlaced.ts"),
		"-f", "lavfi", "-i", "testsrc2=size=720x480:rate=60000/1001",
		"-vf", "tinterlace=mode=interleave_top,setfield=tff", "-t", "4",
		"-c:v", "mpeg2video", "-flags", "+ildct+ilme", "-b:v", "8M", "-an")
}

// hdrClip is 4 s of HEVC Main10 at 1280x720, tagged HDR10 (BT.2020, PQ).
func hdrClip(t *testing.T) string {
	t.Helper()
	return makeClip(t, filepath.Join(t.TempDir(), "hdr.mkv"),
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24", "-t", "4",
		"-pix_fmt", "yuv420p10le", "-c:v", "libx265", "-preset", "ultrafast",
		"-x265-params", "log-level=none:keyint=48:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc",
		"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc")
}

// videoJob is the library job on input with no audio, at w x h and rate.
func videoJob(t *testing.T, input string, w, h int, rate video.Rational) video.Job {
	t.Helper()
	job := library(t, input)
	job.Audio = nil
	job.Video.Width, job.Video.Height, job.Video.FrameRate = w, h, rate
	return job
}

func TestAnInterlacedSourceComesOutProgressive(t *testing.T) {
	// Finding D's channels: Plex asks no filter for it on the Pi, so the
	// pool deinterlaces whatever its source says is interlaced. At the
	// source's own size, so the detector sees the weave unscaled.
	need(t)
	in := interlacedClip(t)
	srcI, srcP := idet(t, in)
	require.Greater(t, srcI, 2*srcP, "the clip is interlaced (%d interlaced, %d progressive)", srcI, srcP)

	out := convert(t, videoJob(t, in, 720, 480, video.Rational{Num: 30000, Den: 1001}), cpu)
	i, p := idet(t, out)
	assert.Greater(t, p, 4*i, "%d frames read interlaced, %d progressive", i, p)
}

func TestAnHDR10SourceComesOutSDR(t *testing.T) {
	// Finding H: Plex on the Pi maps HDR10 to 8 bits with no tone map; the
	// pool tone-maps it to BT.709 SDR, tagged so.
	need(t)
	out := convert(t, videoJob(t, hdrClip(t), 720, 404, video.Rational{Num: 24, Den: 1}), cpu)
	assertSDR(t, out)
}

func assertSDR(t *testing.T, path string) {
	t.Helper()
	b, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries",
		"stream=pix_fmt,color_primaries,color_transfer,color_space", "-of", "json", path).Output()
	require.NoError(t, err)
	var p struct {
		Streams []struct {
			PixFmt    string `json:"pix_fmt"`
			Primaries string `json:"color_primaries"`
			Transfer  string `json:"color_transfer"`
			Space     string `json:"color_space"`
		} `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(b, &p))
	require.Len(t, p.Streams, 1)
	s := p.Streams[0]
	assert.Equal(t, "yuv420p", s.PixFmt)
	assert.Equal(t, "bt709", s.Primaries)
	assert.Equal(t, "bt709", s.Transfer)
	assert.Equal(t, "bt709", s.Space)
}
