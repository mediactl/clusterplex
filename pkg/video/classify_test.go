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

	audioOnly, _ := edit(t, func(a []string) []string {
		a = replaceArg(a, "libx264", "copy")
		for i, v := range a {
			if strings.HasPrefix(v, "[0:0]scale") {
				a = append(a[:i-1], a[i+1:]...) // the video's -filter_complex
				break
			}
		}
		return replaceArg(a, "[1]", "0:0")
	})
	require.False(t, video.IsConversion(audioOnly), "converting the audio alone is no video conversion")

	unknown, _ := edit(t, func(a []string) []string { return replaceArg(a, "libx264", "foo") })
	require.True(t, video.IsConversion(unknown), "an encoder it does not know is refused, never run on Plex's node")
}
