package video_test

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/video"
)

// dashJobs are the 44 Plex Web DASH jobs pkg/remux was built from: every
// one copies its video.
func dashJobs(t *testing.T) [][]string {
	t.Helper()
	f, err := os.Open("../remux/testdata/plex-dash-jobs.log")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var out [][]string
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		_, cmd, ok := strings.Cut(s.Text(), `"/usr/lib/plexmediaserver/Plex Transcoder" `)
		if ok {
			out = append(out, shellWords(cmd))
		}
	}
	require.NoError(t, s.Err())
	require.NotEmpty(t, out)
	return out
}

// shellWords splits the log's command line: double-quoted words, else
// spaces.
func shellWords(s string) []string {
	var out []string
	var b strings.Builder
	quoted, any := false, false
	for _, r := range s {
		switch {
		case r == '"':
			quoted, any = !quoted, true
		case r == ' ' && !quoted:
			if any {
				out = append(out, b.String())
			}
			b.Reset()
			any = false
		default:
			b.WriteRune(r)
			any = true
		}
	}
	if any {
		out = append(out, b.String())
	}
	return out
}

func TestOnlyAVideoConversionIsOne(t *testing.T) {
	for _, args := range dashJobs(t) {
		require.False(t, video.IsConversion(args), "a browser remux copies its video: %v", args)
	}
	for _, c := range fixtures(t) {
		require.True(t, video.IsConversion(c.Args), c.Shape)
	}

	require.False(t, video.IsConversion(audioOnly(t, "aac")), "converting the audio alone is no video conversion")

	unknown, _ := edit(t, func(a []string) []string { return replaceArg(a, "libx264", "foo") })
	require.True(t, video.IsConversion(unknown), "an encoder it does not know is refused, never run on Plex's node")
}

// audioOnly is library-hls with its video copied and its audio encoded by
// enc.
func audioOnly(t *testing.T, enc string) []string {
	t.Helper()
	a, _ := edit(t, func(a []string) []string {
		a = replaceArg(a, "libx264", "copy")
		for i, v := range a {
			if strings.HasPrefix(v, "[0:0]scale") {
				a = append(a[:i-1], a[i+1:]...) // the video's -filter_complex
				break
			}
		}
		a = replaceArg(a, "[1]", "0:0")
		return replaceArg(a, "aac", enc)
	})
	return a
}

// Plex's own libavcodec (ghcr.io/mediactl/cluster-plex:f73c548,
// usr/lib/plexmediaserver/lib/libavcodec.so.60) is configured
// --disable-encoders and then these, by the names a job's argv uses; aac,
// ac3, eac3, libmp3lame and libx264 it downloads.
var (
	plexPlaybackEncoders = []string{"h264_nvenc", "h264_vaapi", "hevc_nvenc", "hevc_vaapi", "libx264"}
	plexOtherEncoders    = []string{
		"aac", "ac3", "eac3", "libmp3lame",
		"alac", "eac3_eae", "flac", "libopus", "libvorbis",
		"pcm_f32be", "pcm_f32le", "pcm_f64be", "pcm_f64le", "pcm_s8", "pcm_s8_planar", "pcm_s16be",
		"pcm_s16be_planar", "pcm_s16le", "pcm_s16le_planar", "pcm_s24be", "pcm_s24le", "pcm_s24le_planar",
		"pcm_s32be", "pcm_s32le", "pcm_s32le_planar", "pcm_u8", "pcm_u16be", "pcm_u16le", "pcm_u24be",
		"pcm_u24le", "pcm_u32be", "pcm_u32le",
		"ass", "dvbsub", "dvdsub", "mov_text", "ssa", "subrip", "text", "webvtt", "xsub",
		"mjpeg", "png", "rawvideo", "wrapped_avframe",
	}
)

func TestPlexsOtherEncodersLeaveAJobAsItIs(t *testing.T) {
	// An E-AC-3 conversion through Plex's EasyAudioEncoder copies its video,
	// as do its subtitle outputs; its image and raw outputs are thumbnails
	// and analyses, which no client plays.
	for _, enc := range plexOtherEncoders {
		require.False(t, video.IsConversion(audioOnly(t, enc)), enc)
	}
	for _, enc := range plexPlaybackEncoders {
		args, _ := edit(t, func(a []string) []string { return replaceArg(a, "libx264", enc) })
		require.True(t, video.IsConversion(args), enc)
	}
}
