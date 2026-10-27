package pipeline_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		if c.Shape != "dash-encode" {
			continue
		}
		j, err := video.Parse(c.Args, c.Env)
		require.NoError(t, err)
		j.Inputs[0].Path = input
		return video.JobFromProto(j.Proto())
	}
	t.Fatal("no dash-encode fixture")
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

	require.Len(t, r.manifests, n+1, "one manifest a segment, and the final one")
	for i := range n {
		assert.Equal(t, 2+2*(i+1), r.before[i], "manifest %d after its segment's files", i)
		assert.Contains(t, r.manifests[i], `type="dynamic"`)
	}
	final := r.manifests[n]
	assert.Contains(t, final, `type="static"`)
	assert.Contains(t, final, `startNumber="1"`)
	assert.Contains(t, final, `codecs="avc1.`)
	assert.Contains(t, final, `codecs="mp4a.40.2"`)
	assert.Contains(t, final, `width="720" height="404"`)
	assert.Equal(t, 2*n, strings.Count(final, "<S "), "every segment of both representations on the timeline")

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
