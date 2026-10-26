package video_test

import (
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/video"
)

func TestKeyframesAreForcedAsPlexsExpressionSays(t *testing.T) {
	const every = 8 * time.Second
	require.True(t, video.KeyframeDue(0, 0, every), "the first frame")
	require.False(t, video.KeyframeDue(40*time.Millisecond, 1, every))
	require.False(t, video.KeyframeDue(7999*time.Millisecond, 1, every))
	require.True(t, video.KeyframeDue(8*time.Second, 1, every))
	require.True(t, video.KeyframeDue(1800*time.Second, 0, every), "a restart's first frame")
}

func TestSegmentNamesFollowThePattern(t *testing.T) {
	require.Equal(t, "media-00007.ts", video.SegmentName("media-%05d.ts", 7))
	require.Equal(t, "media-123456.ts", video.SegmentName("media-%05d.ts", 123456))
}

// posts are the segment-list bodies Plex's transcoder sent, in order.
func posts(t *testing.T, shape string) []string {
	t.Helper()
	b, err := os.ReadFile("testdata/segment-lists/" + shape + ".txt")
	require.NoError(t, err)
	var out []string
	for _, block := range strings.Split(string(b), "-- POST\n")[1:] {
		out = append(out, block)
	}
	require.NotEmpty(t, out)
	return out
}

// times reads a list line back: name, start, end, each stream's end.
func times(t *testing.T, line string) (name string, start, end time.Duration, streams []time.Duration) {
	t.Helper()
	f := strings.Split(strings.TrimPrefix(line, "#"), ",")
	require.GreaterOrEqual(t, len(f), 3, line)
	sec := func(v string) time.Duration {
		s, err := strconv.ParseFloat(v, 64)
		require.NoError(t, err)
		return time.Duration(math.Round(s*1e6)) * time.Microsecond
	}
	for _, v := range f[3:] {
		streams = append(streams, sec(v))
	}
	return f[0], sec(f[1]), sec(f[2]), streams
}

func TestSegmentListLinesArePlexs(t *testing.T) {
	l := video.NewSegmentList(5)
	for i, body := range posts(t, "library-hls") {
		lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
		last := lines[len(lines)-1]
		name, start, end, streams := times(t, last)
		var got string
		if strings.HasPrefix(last, "#") {
			got = l.Opened(name, start, len(streams))
		} else {
			st := video.SegmentTimes{Name: name, Start: start, End: end}
			for j, e := range streams {
				st.Streams = append(st.Streams, video.Span{Stream: j, End: e})
			}
			got = l.Finished(st)
		}
		require.Equal(t, body, got, "POST %d", i)
	}
}

func TestCutsFallWhereFindingRSays(t *testing.T) {
	// The library job's frames at 24000/1001, keyframes forced every 8 s,
	// cut by the rule; the captured segments start where it cuts.
	s := video.Segmenting{Duration: 8 * time.Second, TimeDelta: 62500 * time.Microsecond}
	var cuts []time.Duration
	forced, cut := 0, 0
	for n := 0; n < 26*192+10; n++ {
		pts := time.Duration(n) * time.Second * 1001 / 24000
		if !video.KeyframeDue(pts, forced, 8*time.Second) {
			continue
		}
		forced++
		if n > 0 && video.CutDue(pts, 0, cut, s) {
			cuts = append(cuts, pts)
			cut++
		}
	}
	var want []time.Duration
	for _, body := range posts(t, "library-hls") {
		lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
		if last := lines[len(lines)-1]; strings.HasPrefix(last, "#") {
			_, start, _, _ := times(t, last)
			want = append(want, start)
		}
	}
	require.Len(t, want, 25)
	for i, w := range want {
		require.InDelta(t, w.Seconds(), cuts[i].Seconds(), 0.001, "segment %d's start", i+1)
	}
}
