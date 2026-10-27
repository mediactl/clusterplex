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
		"dash-encode": video.OutputDASH, "livetv-dash": video.OutputDASH, "download": video.OutputFile,
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

// setValue sets option opt's value.
func setValue(args []string, opt, v string) []string {
	for i := range len(args) - 1 {
		if args[i] == opt {
			args[i+1] = v
		}
	}
	return args
}

func replaceArg(args []string, old, new string) []string {
	for i, a := range args {
		if a == old {
			args[i] = new
		}
	}
	return args
}

func TestTheLiveTVDashJobParses(t *testing.T) {
	// A browser's Live TV tune (talos, 2026-10-27): Plex's live session as
	// an HLS playlist from its first segment, 1 s DASH segments, the audio
	// copied.
	c := fixture(t, "livetv-dash")
	j, err := video.Parse(c.Args, c.Env)
	require.NoError(t, err)
	require.Equal(t, video.OutputDASH, j.Output)
	require.Len(t, j.Inputs, 1)
	in := j.Inputs[0]
	require.True(t, in.Streamed && in.Live, "the manager follows the session's playlist")
	require.Equal(t, "mpegts", in.Format, "its segments reach the worker as one MPEG-TS")
	require.Contains(t, in.Source, "/livetv/sessions/79290666-a30d-4acd-a687-e333d1b9a715/t55ke0psb8hxtg2d4ax4hml9/index.m3u8?offset=6.873533")
	require.Equal(t, video.VideoOut{
		Stream: 0, Width: 1280, Height: 720, PixFmt: "nv12", CRF: 16,
		MaxRate: 9_000_000, BufSize: 18_000_000, FrameRate: video.Rational{Num: 30000, Den: 1001},
		KeyframeEvery: time.Second,
	}, j.Video)
	require.Equal(t, &video.AudioOut{Stream: 1, Copy: true}, j.Audio)
	require.Equal(t, video.Segmenting{Duration: time.Second, Pattern: "chunk-stream$RepresentationID$-$Number%05d$.m4s", StartNumber: 1, ListSize: 5}, j.Segments)
	require.Equal(t, "b3ub8tqf4v85wnlo2v156fuh", j.Session)
	require.Contains(t, j.ManifestURL, "/manifest?X-Plex-Http-Pipeline=infinite")

	wire := video.JobFromProto(j.Proto())
	require.False(t, wire.Inputs[0].Live, "the worker never sees where the input comes from")
	require.Empty(t, wire.Inputs[0].Source)
	require.Equal(t, "mpegts", wire.Inputs[0].Format)
}

func TestALiveInputOutsideItsShapeIsRefused(t *testing.T) {
	for name, edit := range map[string]func([]string) []string{
		"another start index": func(a []string) []string { return setValue(a, "-live_start_index", "-3") },
		"no playlist": func(a []string) []string {
			for i, v := range a {
				if strings.Contains(v, "index.m3u8") {
					a[i] = strings.Replace(v, "index.m3u8", "00007.ts", 1)
				}
			}
			return a
		},
		"copypriorss 1": func(a []string) []string { return setValue(a, "-copypriorss:1", "1") },
	} {
		t.Run(name, func(t *testing.T) {
			c := fixture(t, "livetv-dash")
			_, err := video.Parse(edit(slices.Clone(c.Args)), c.Env)
			require.ErrorIs(t, err, video.ErrShape)
		})
	}
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

func TestTheHDRJobParsesAsPlexAsksIt(t *testing.T) {
	// Finding H: with this server's settings Plex maps HDR10 to 8 bits
	// through its scale and format alone, with no tone-map filter, and cuts
	// 5 s segments.
	j, err := video.Parse(fixture(t, "hdr-sdr").Args, fixture(t, "hdr-sdr").Env)
	require.NoError(t, err)
	require.False(t, j.Video.ToneMap)
	require.Equal(t, 1280, j.Video.Width)
	require.Equal(t, 544, j.Video.Height)
	require.Equal(t, 5*time.Second, j.Video.KeyframeEvery)
	require.Equal(t, 5*time.Second, j.Segments.Duration)
	require.Equal(t, 1, j.Audio.Stream)
}

func TestTheDashEncodeJobParses(t *testing.T) {
	c := fixture(t, "dash-encode")
	j, err := video.Parse(c.Args, c.Env)
	require.NoError(t, err)
	require.Equal(t, video.OutputDASH, j.Output)
	require.Equal(t, video.Segmenting{
		Duration: 8 * time.Second, StartNumber: 1, Pattern: "chunk-stream$RepresentationID$-$Number%05d$.m4s", ListSize: 5,
	}, j.Segments)
	require.Contains(t, j.ManifestURL, "/manifest?X-Plex-Http-Pipeline=infinite")
	require.Empty(t, j.SegmentListURL)
	require.Equal(t, 720, j.Video.Width)
	require.Equal(t, 404, j.Video.Height)
	require.Equal(t, &video.AudioOut{Stream: 1, Channels: 2, SampleRate: 96000, BitRate: 232_000, Language: "eng"}, j.Audio)
}

func TestTheImageBurnInJobParses(t *testing.T) {
	// Finding B: Plex overlays a PGS track, scaled to the output's size, on
	// the scaled video.
	c := fixture(t, "burn-image")
	j, err := video.Parse(c.Args, c.Env)
	require.NoError(t, err)
	require.Equal(t, &video.BurnIn{Input: 0, Stream: 3, Image: true}, j.Burn)
	require.Equal(t, 0, j.Video.Stream)
	require.Equal(t, 720, j.Video.Width)
	require.Equal(t, 384, j.Video.Height)
}

func TestTheTextBurnInJobParses(t *testing.T) {
	// Finding B: PMS copies the sidecar into the session's directory and
	// burns it in with its own inlineass, whose style the worker hands to
	// libass; Plex's extra subtitle output to null is dropped. Plex's sizes
	// are on a 1080-line picture, libass's for a SubRip on 288 lines.
	c := fixture(t, "burn-text-sidecar")
	j, err := video.Parse(c.Args, c.Env)
	require.NoError(t, err)
	require.Len(t, j.Inputs, 2)
	require.True(t, j.Inputs[1].Streamed)
	require.Equal(t, "temp-0.srt", j.Inputs[1].Name)
	require.Equal(t, c.Cwd+"/temp-0.srt", j.Inputs[1].Source)
	require.Equal(t, &video.BurnIn{
		Input: 1, Stream: 0,
		Style: "ScaledBorderAndShadow=yes,FontName=Noto Sans Medium,Bold=500,PrimaryColour=&H00FFFFFF," +
			"OutlineColour=&H00020713,BackColour=&HCC000000,Fontsize=14,Outline=0.69,Shadow=0.45",
	}, j.Burn)
	require.Equal(t, 0, j.Video.Stream)
	require.Equal(t, 2, j.Audio.Stream)
	require.Equal(t, video.OutputHLS, j.Output)
}

func TestTheEmbeddedTextBurnInJobParses(t *testing.T) {
	// Finding B: an embedded ASS track is named by its index (-map_inlineass
	// 0:9) and keeps its own styles: inlineass carries no overrides.
	c := fixture(t, "burn-text-embedded")
	j, err := video.Parse(c.Args, c.Env)
	require.NoError(t, err)
	require.Len(t, j.Inputs, 1)
	require.Equal(t, &video.BurnIn{Input: 0, Stream: 9}, j.Burn)
	require.Equal(t, 5*time.Second, j.Segments.Duration)
}

func TestABurnInOutsideItsShapeIsRefused(t *testing.T) {
	edited := func(shape string, fn func([]string) []string) ([]string, map[string]string) {
		c := fixture(t, shape)
		return fn(slices.Clone(c.Args)), c.Env
	}
	inArg := func(sub, old, new string) func([]string) []string {
		return func(a []string) []string {
			for i, v := range a {
				if strings.Contains(v, sub) {
					a[i] = strings.Replace(v, old, new, 1)
				}
			}
			return a
		}
	}
	for name, tc := range map[string]struct {
		shape string
		edit  func([]string) []string
		says  string
	}{
		"a subtitle scaled to another size": {"burn-image", inArg("overlay", "scale=720:384", "scale=1920:1080"), "1920x1080"},
		"an overlay of a second video":      {"burn-image", inArg("overlay", "[0:3]", "[0:0]"), "overlay"},
		"inlineass without its map": {"burn-text-sidecar", func(a []string) []string {
			i := slices.Index(a, "-map_inlineass")
			return slices.Delete(a, i, i+2)
		}, "inlineass"},
		"an inlineass option it does not know": {"burn-text-sidecar", inArg("inlineass", "shadow=", "blur=1:shadow="), "blur"},
		"a subtitle-relative map into the library file": {"burn-text-embedded", func(a []string) []string {
			return replaceArg(a, "0:9", "0:s:2")
		}, "0:s:2"},
		"a null output of another stream": {"burn-text-sidecar", func(a []string) []string {
			a[len(a)-6] = "0:s:0" // the null output's -map, not -map_inlineass's
			return a
		}, "0:s:0"},
	} {
		t.Run(name, func(t *testing.T) {
			args, env := edited(tc.shape, tc.edit)
			_, err := video.Parse(args, env)
			require.Error(t, err)
			require.True(t, errors.Is(err, video.ErrShape), "%v is not ErrShape", err)
			require.Contains(t, err.Error(), tc.says)
		})
	}
}
