package pipeline_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/remux"
	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/pipeline"
)

// dashRecorder keeps what a DASH run sends: its files and manifests, and
// how many files had arrived as each manifest did.
type dashRecorder struct {
	recorder
	mu        sync.Mutex
	files     map[string][]byte
	manifests []string
	before    []int
}

func (r *dashRecorder) File(name string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.files == nil {
		r.files = map[string][]byte{}
	}
	r.files[name] = append([]byte(nil), data...)
	return nil
}

func (r *dashRecorder) Manifest(mpd []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manifests = append(r.manifests, string(mpd))
	r.before = append(r.before, len(r.files))
	return nil
}

// dashJob is the captured dash-encode job on input, its audio stream 1.
func dashJob(t *testing.T, input string) video.Job {
	t.Helper()
	j := jobOf(t, "dash-encode")
	j.Inputs[0].Path = input
	return j
}

// jobOf is the captured job of shape as the worker gets it.
func jobOf(t *testing.T, shape string) video.Job {
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
		return video.JobFromProto(j.Proto())
	}
	t.Fatalf("no %s fixture", shape)
	return video.Job{}
}

func TestADashEncodeKeepsTheRemuxPoolsContract(t *testing.T) {
	// Finding W: Plex Web's converted DASH is the remux pool's contract
	// with an encoded video: each representation's init and chunks under
	// its names, numbered from -skip_to_segment, every chunk on the
	// manifest only after its files, and a static manifest at the end.
	need(t)
	job := dashJob(t, clip(t, 20, "mkv"))
	r := &dashRecorder{}
	require.NoError(t, pipeline.Run(t.Context(), job, cpu, r))

	for _, name := range []string{remux.InitFile(0), remux.InitFile(1)} {
		require.Contains(t, r.files, name)
	}
	n := 0
	for ; ; n++ {
		if _, ok := r.files[remux.ChunkFile(0, n+1)]; !ok {
			break
		}
		require.Contains(t, r.files, remux.ChunkFile(1, n+1), "segment %d's audio", n+1)
	}
	require.Equal(t, 3, n, "20 s in 8 s segments")
	require.Len(t, r.files, 2+2*n, "no other file")

	// dashenc's: a running manifest a segment but the last, whose files the
	// final, static one lists.
	require.Len(t, r.manifests, n, "one manifest a segment, the last the final one")
	for i := range n - 1 {
		assert.Equal(t, 2+2*(i+1), r.before[i], "manifest %d after its segment's files", i)
		assert.Contains(t, r.manifests[i], `type="dynamic"`)
	}
	assert.Equal(t, 2+2*n, r.before[n-1], "the final manifest after every file")
	final := r.manifests[n-1]
	assert.Contains(t, final, `type="static"`)
	assert.Contains(t, final, `startNumber="1"`)
	assert.Contains(t, final, `codecs="avc1.`)
	assert.Contains(t, final, `codecs="mp4a.40.2"`)
	assert.Contains(t, final, `width="720" height="404"`)
	assert.Equal(t, 2*n, segmentsListed(final), "every segment of both representations on the timeline")

	// Each video chunk, after its init, starts on a keyframe.
	dir := t.TempDir()
	for i := 1; i <= n; i++ {
		path := filepath.Join(dir, fmt.Sprintf("v%d.mp4", i))
		require.NoError(t, os.WriteFile(path, append(append([]byte(nil), r.files[remux.InitFile(0)]...), r.files[remux.ChunkFile(0, i)]...), 0o644))
		out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "frame=key_frame,pict_type",
			"-read_intervals", "%+#1", "-of", "csv=p=0", path).Output()
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(strings.TrimSpace(string(out)), "1"), "chunk %d starts %q", i, out)
	}
}

func TestALiveTVDashCopiesItsAudio(t *testing.T) {
	// livetv-dash: Plex's live session as one MPEG-TS, 1 s segments, the
	// AAC copied. ADTS in MPEG-TS names no AudioSpecificConfig, so the audio
	// init carries the one its first header describes (48 kHz mono here),
	// the chunks decode, and the manifest lists the last -window_size (5)
	// segments, as FFmpeg's live DASH does.
	need(t)
	job := jobOf(t, "livetv-dash")
	data := liveTS(t, 12, 100)
	o := cpu
	o.Inputs = func(int) io.Reader { return bytes.NewReader(data) }
	r := &dashRecorder{}
	require.NoError(t, pipeline.Run(t.Context(), job, o, r))

	ainit := r.files[remux.InitFile(1)]
	require.NotEmpty(t, ainit)
	i := bytes.Index(ainit, []byte("esds"))
	require.Positive(t, i, "the audio init has an esds")
	// DecoderSpecificInfo (tag 5), its length in the muxer's four-byte form.
	assert.True(t, bytes.Contains(ainit[i:], []byte{0x05, 0x80, 0x80, 0x80, 0x02, 0x11, 0x88}),
		"its decoder config is AAC-LC 48 kHz mono: % x", ainit[i:min(len(ainit), i+48)])

	n := 0
	for ; ; n++ {
		if _, ok := r.files[remux.ChunkFile(0, n+1)]; !ok {
			break
		}
	}
	require.InDelta(t, 12, n, 1, "12 s in 1 s segments")
	for _, m := range r.manifests[:len(r.manifests)-1] {
		assert.Equal(t, 2, strings.Count(m, "<S t="), "no timeline gap: only each representation's first segment names its start: %s", m)
	}
	final := r.manifests[len(r.manifests)-1]
	assert.Contains(t, final, `codecs="mp4a.40.2"`)
	assert.Contains(t, final, `audioSamplingRate="48000"`)
	assert.Contains(t, final, fmt.Sprintf(`startNumber="%d"`, n-4), "the window's first segment")
	assert.LessOrEqual(t, strings.Count(final, "<S "), 10, "five segments of each representation, repeats folded")
	assert.NotContains(t, final, `lang=""`, "no language is named when the stream has none")
	assert.NotContains(t, final, `bandwidth="0"`)
	for _, m := range r.manifests[:len(r.manifests)-1] {
		assert.Contains(t, m, `timeShiftBufferDepth="PT5.`, "a running live manifest's window, as Plex's")
	}

	path := filepath.Join(t.TempDir(), "audio.mp4")
	all := append([]byte(nil), ainit...)
	for k := 1; k <= n; k++ {
		all = append(all, r.files[remux.ChunkFile(1, k)]...) // the last may be none
	}
	require.NoError(t, os.WriteFile(path, all, 0o644))
	out, err := exec.Command("ffmpeg", "-v", "error", "-xerror", "-i", path, "-f", "null", "-").CombinedOutput()
	require.NoError(t, err, "the copied audio decodes: %s", out)
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	require.NoError(t, err)
	assert.Contains(t, []string{"11", "12"}, strings.SplitN(strings.TrimSpace(string(probe)), ".", 2)[0], "its duration")
}

// segmentsListed counts a manifest's segments, each <S> one and its r more.
func segmentsListed(mpd string) int {
	n := 0
	for _, entry := range regexp.MustCompile(`<S [^>]*/>`).FindAllString(mpd, -1) {
		n++
		if m := regexp.MustCompile(`r="(\d+)"`).FindStringSubmatch(entry); m != nil {
			r, _ := strconv.Atoi(m[1])
			n += r
		}
	}
	return n
}
