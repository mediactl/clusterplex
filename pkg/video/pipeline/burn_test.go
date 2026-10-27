package pipeline_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/pipeline"
)

// shapeJob is a captured job (pkg/video's fixture) of shape on input,
// without audio, through the wire as a worker gets it.
func shapeJob(t *testing.T, shape, input string) video.Job {
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
		if c.Shape != shape {
			continue
		}
		j, err := video.Parse(c.Args, c.Env)
		require.NoError(t, err)
		j.Inputs[0].Path = input
		j.Audio = nil
		return video.JobFromProto(j.Proto())
	}
	t.Fatalf("no %s fixture", shape)
	return video.Job{}
}

// fontsDir holds Noto Sans Medium, the font Plex burns text in.
func fontsDir(t *testing.T) string {
	t.Helper()
	const dir = "/usr/share/fonts/noto"
	if _, err := os.Stat(filepath.Join(dir, "NotoSans-Medium.ttf")); err != nil {
		skipOrFail(t, "no Noto Sans Medium in %s", dir)
	}
	return dir
}

// grayClip is 6 s of flat gray at 1920x1080, 24 fps, H.264 in Matroska:
// a background on which a subtitle is all that changes.
func grayClip(t *testing.T) string {
	t.Helper()
	return makeClip(t, filepath.Join(t.TempDir(), "gray.mkv"),
		"-f", "lavfi", "-i", "color=c=gray:size=1920x1080:rate=24", "-t", "6",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
}

func srtFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("1\n00:00:01,000 --> 00:00:03,000\nHELLO\n\n"), 0o644))
	return path
}

// frame is output frame n of a converted file, raw yuv420p.
func frame(t *testing.T, path string, n, w, h int) []byte {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-vf", "select=eq(n\\,"+strconv.Itoa(n)+")",
		"-frames:v", "1", "-f", "rawvideo", "-pix_fmt", "yuv420p", "-").Output()
	require.NoError(t, err)
	require.Len(t, out, w*h*3/2, "frame %d", n)
	return out
}

// changedBelow is the fraction of the bottom fifth's luma that differs by
// more than 32 levels between two frames: where a subtitle is drawn.
func changedBelow(a, b []byte, w, h int) float64 {
	from := h * 4 / 5
	n, changed := 0, 0
	for y := from; y < h; y++ {
		for x := range w {
			d := int(a[y*w+x]) - int(b[y*w+x])
			if d > 32 || d < -32 {
				changed++
			}
			n++
		}
	}
	return float64(changed) / float64(n)
}

// assertTextBurned converts job with and without its burn-in and checks
// that the subtitle is drawn at 2 s and gone by 5 s.
func assertTextBurned(t *testing.T, job video.Job, o pipeline.Options) {
	t.Helper()
	plain := job
	plain.Burn = nil
	w, h := job.Video.Width, job.Video.Height
	with, without := convert(t, job, o), convert(t, plain, o)
	at2 := changedBelow(frame(t, with, 48, w, h), frame(t, without, 48, w, h), w, h)
	at5 := changedBelow(frame(t, with, 120, w, h), frame(t, without, 120, w, h), w, h)
	assert.Greater(t, at2, 0.005, "HELLO is drawn at 2 s")
	assert.Less(t, at5, 0.0005, "and gone by 5 s")
}

// streamedOptions are CPU options whose input 1 is the file at path,
// streamed as the manager streams it.
func streamedOptions(t *testing.T, path string) pipeline.Options {
	t.Helper()
	o := cpu
	o.FontsDir = fontsDir(t)
	o.InputsDir = t.TempDir()
	o.Inputs = func(i int) io.Reader {
		if i != 1 {
			return bytes.NewReader(nil)
		}
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		return bytes.NewReader(b)
	}
	return o
}

func sidecarJob(t *testing.T, clip string) video.Job {
	job := shapeJob(t, "burn-text-sidecar", clip)
	job.Video.Width, job.Video.Height = 720, 404
	return job
}

func TestATextSubtitleIsBurnedIn(t *testing.T) {
	// burn-text-sidecar: the SRT PMS copied into the session, streamed to
	// the worker as input 1, drawn in Plex's style.
	need(t)
	srt := srtFile(t, t.TempDir(), "temp-0.srt")
	assertTextBurned(t, sidecarJob(t, grayClip(t)), streamedOptions(t, srt))
}

func TestAnEmbeddedTextTrackIsBurnedIn(t *testing.T) {
	// burn-text-embedded: an ASS track in the library file, in its own style.
	need(t)
	dir := t.TempDir()
	mkv := makeClip(t, filepath.Join(dir, "with-ass.mkv"), "-i", grayClip(t), "-i", srtFile(t, dir, "s.srt"),
		"-map", "0:v", "-map", "1:s", "-c:v", "copy", "-c:s", "ass")
	job := shapeJob(t, "burn-text-embedded", mkv)
	job.Burn.Stream = 1
	job.Video.Width, job.Video.Height = 720, 404
	o := cpu
	o.FontsDir, o.InputsDir = fontsDir(t), t.TempDir()
	assertTextBurned(t, job, o)
}

func TestAPathWithAQuoteAndAColonBurnsIn(t *testing.T) {
	// A filter argument never carries the subtitle's own name: it is held
	// under a name of the worker's.
	need(t)
	srt := srtFile(t, t.TempDir(), "Schindler's List: Part 1, (1993)/Schindler's List: Part 1, (1993).en.srt")
	job := sidecarJob(t, grayClip(t))
	job.Inputs[1].Name = filepath.Base(srt)
	assertTextBurned(t, job, streamedOptions(t, srt))
}

func TestAStreamedSubtitleIsHeldInMemoryOnly(t *testing.T) {
	// The subtitle is written under InputsDir/<session>/ (the pod's
	// memory-backed emptyDir) and gone once the run ends.
	need(t)
	o := streamedOptions(t, srtFile(t, t.TempDir(), "temp-0.srt"))
	job := sidecarJob(t, grayClip(t))
	session := filepath.Join(o.InputsDir, job.Session)
	var during []os.DirEntry
	r := &recorder{onSeg: func(pipeline.Segment) {
		if during == nil {
			during, _ = os.ReadDir(session)
		}
	}}
	require.NoError(t, pipeline.Run(t.Context(), job, o, r))
	assert.NotEmpty(t, during, "the subtitle is in %s while the job runs", session)
	_, err := os.Stat(session)
	assert.True(t, os.IsNotExist(err), "%s is gone after the run", session)
}

func TestAnImageSubtitleIsComposited(t *testing.T) {
	// burn-image: a PGS box (red, 64x32 at (100,600) on the 1920x1080
	// canvas) on a 720x384 output lands scaled, red during its span and
	// untouched outside it.
	need(t)
	box := red
	box.start, box.end, box.x, box.y, box.w, box.h = time.Second, 3*time.Second, 100, 600, 64, 32
	// -copyts: ffmpeg would otherwise start the .sup's times at zero.
	mkv := makeClip(t, filepath.Join(t.TempDir(), "with-pgs.mkv"), "-copyts", "-i", grayClip(t), "-i", writeSup(t, 1920, 1080, box),
		"-map", "0:v", "-map", "1:s", "-c", "copy")
	job := shapeJob(t, "burn-image", mkv)
	job.Burn.Stream = 1
	plain := job
	plain.Burn = nil
	w, h := job.Video.Width, job.Video.Height
	with, without := convert(t, job, cpu), convert(t, plain, cpu)

	// The box's centre, scaled: (132, 616) * 720/1920, 384/1080.
	uv := func(f []byte, x, y int) (u, v int) {
		i := (y/2)*(w/2) + x/2
		return int(f[w*h+i]), int(f[w*h+w*h/4+i])
	}
	cx, cy := 132*w/1920, 616*h/1080
	at2, plain2 := frame(t, with, 48, w, h), frame(t, without, 48, w, h)
	u, v := uv(at2, cx, cy)
	assert.Greater(t, v, u+40, "red at 2 s: U %d V %d", u, v)
	ou, ov := uv(at2, cx+80, cy)
	pu, pv := uv(plain2, cx+80, cy)
	assert.InDelta(t, pu, ou, 4, "beside the box, untouched")
	assert.InDelta(t, pv, ov, 4, "beside the box, untouched")
	u5, v5 := uv(frame(t, with, 120, w, h), cx, cy)
	u0, v0 := uv(frame(t, without, 120, w, h), cx, cy)
	assert.InDelta(t, u0, u5, 4, "untouched at 5 s")
	assert.InDelta(t, v0, v5, 4, "untouched at 5 s")
}
