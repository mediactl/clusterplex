package video_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/video"
)

// captured is one Plex Transcoder job recorded on talos (hack/capture-video-jobs.sh).
type captured struct {
	Shape string            `json:"shape"`
	Args  []string          `json:"args"`
	Env   map[string]string `json:"env"`
	Cwd   string            `json:"cwd"`
}

func fixtures(t *testing.T) []captured {
	t.Helper()
	f, err := os.Open("testdata/plex-video-jobs.jsonl")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var out []captured
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		var c captured
		require.NoError(t, json.Unmarshal(s.Bytes(), &c))
		out = append(out, c)
	}
	require.NoError(t, s.Err())
	return out
}

func fixture(t *testing.T, shape string) captured {
	t.Helper()
	for _, c := range fixtures(t) {
		if c.Shape == shape {
			return c
		}
	}
	t.Fatalf("no %s fixture", shape)
	return captured{}
}

func TestTheLibraryHLSJobParses(t *testing.T) {
	c := fixture(t, "library-hls")
	j, err := video.Parse(c.Args, c.Env)
	require.NoError(t, err)

	require.Equal(t, video.OutputHLS, j.Output)
	require.Len(t, j.Inputs, 1)
	require.False(t, j.Inputs[0].Streamed)
	require.True(t, strings.HasPrefix(j.Inputs[0].Path, "/media/movies/Grizzly Man (2005) {tmdb-501}/"), j.Inputs[0].Path)
	require.Zero(t, j.Start)
	require.Equal(t, video.VideoOut{
		Stream: 0, Width: 720, Height: 404, PixFmt: "nv12", CRF: 17,
		MaxRate: 1_818_000, BufSize: 3_636_000, FrameRate: video.Rational{Num: 24000, Den: 1001},
		KeyframeEvery: 8 * time.Second,
	}, j.Video)
	require.Equal(t, &video.AudioOut{Stream: 2, Channels: 2, SampleRate: 96000, BitRate: 232_000, Language: "eng"}, j.Audio)
	require.Nil(t, j.Burn)
	require.Equal(t, video.Segmenting{
		Duration: 8 * time.Second, Pattern: "media-%05d.ts", ListSize: 5,
		TimeDelta: 62500 * time.Microsecond,
	}, j.Segments)
	require.Equal(t, "REDACTED", j.Token)
	require.Equal(t, "capture-46986", j.Session)
	require.Contains(t, j.SegmentListURL, "/manifest?X-Plex-Http-Pipeline=infinite")
	require.True(t, strings.HasSuffix(j.ProgressURL, "/progress"))
}

func TestEveryCapturedVideoJobParses(t *testing.T) {
	want := map[string]video.OutputKind{
		"dash-encode": video.OutputDASH, "download": video.OutputFile,
		"optimize": video.OutputFile, "dvr-convert": video.OutputFile,
	}
	for _, c := range fixtures(t) {
		t.Run(c.Shape, func(t *testing.T) {
			j, err := video.Parse(c.Args, c.Env)
			require.NoError(t, err)
			kind, ok := want[c.Shape]
			if !ok {
				kind = video.OutputHLS
			}
			require.Equal(t, kind, j.Output)
			if strings.HasPrefix(c.Shape, "livetv") {
				require.True(t, j.Inputs[0].Streamed, "Live TV's input is Plex's live session, streamed by the manager")
			}
		})
	}
}

// edit is the library job's argv with one change made by fn.
func edit(t *testing.T, fn func([]string) []string) ([]string, map[string]string) {
	t.Helper()
	c := fixture(t, "library-hls")
	return fn(slices.Clone(c.Args)), c.Env
}

func replaceArg(args []string, old, new string) []string {
	for i, a := range args {
		if a == old {
			args[i] = new
		}
	}
	return args
}

func TestAnUnknownVideoOptionIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func([]string) []string
		says string
	}{
		"a channel count": {func(a []string) []string { return slices.Insert(a, len(a)-1, "-ac", "6") }, "-ac"},
		"an HEVC output":  {func(a []string) []string { return replaceArg(a, "libx264", "libx265") }, "libx265"},
		"a filter it does not know": {func(a []string) []string {
			for i, v := range a {
				if strings.HasPrefix(v, "[0:0]scale") {
					a[i] = strings.Replace(v, "[1]", ",drawtext=text=x[1]", 1)
				}
			}
			return a
		}, "drawtext"},
		"another muxer":         {func(a []string) []string { return replaceArg(a, "ssegment", "flv") }, "flv"},
		"a simple filter":       {func(a []string) []string { return slices.Insert(a, len(a)-1, "-vf", "yadif") }, "-vf"},
		"another segment muxer": {func(a []string) []string { return replaceArg(a, "mpegts", "mp4") }, "mp4"},
	} {
		t.Run(name, func(t *testing.T) {
			args, env := edit(t, tc.edit)
			_, err := video.Parse(args, env)
			require.Error(t, err)
			require.True(t, errors.Is(err, video.ErrShape), "%v is not ErrShape", err)
			require.Contains(t, err.Error(), tc.says)
		})
	}
}

func TestAnIrregularFrameRateIsPlexsRate(t *testing.T) {
	for in, want := range map[string]video.Rational{
		"23.975999999999999": {Num: 24000, Den: 1001},
		"29.97002997":        {Num: 30000, Den: 1001},
		"59.94005994":        {Num: 60000, Den: 1001},
		"25":                 {Num: 25, Den: 1},
		"12.5":               {Num: 25, Den: 2},
	} {
		args, env := edit(t, func(a []string) []string { return replaceArg(a, "23.975999999999999", in) })
		j, err := video.Parse(args, env)
		require.NoError(t, err, in)
		require.Equal(t, want, j.Video.FrameRate, in)
	}
}
