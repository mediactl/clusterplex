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
	out := strings.Split(string(b), "-- POST\n")[1:]
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
	for _, shape := range []string{"library-hls", "library-hls-seek"} {
		t.Run(shape, func(t *testing.T) { replayList(t, shape) })
	}
}

func replayList(t *testing.T, shape string) {
	l := video.NewSegmentList(5)
	for i, body := range posts(t, shape) {
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
	// A job's output frames on the constant-rate grid anchored at its -ss
	// (24000/1001), keyframes forced every 8 s of that grid, cut by the rule
	// with the counter at -segment_start_number: the captured segments start
	// where it cuts. Plex lists each time shifted by its first frame's offset
	// from -ss (8.3 ms after the restart), within a third of a frame.
	for _, tc := range []struct {
		shape string
		ss    time.Duration
		first int
		cuts  int
	}{{"library-hls", 0, 0, 25}, {"library-hls-seek", 1800 * time.Second, 225, 21}} {
		t.Run(tc.shape, func(t *testing.T) {
			s := video.Segmenting{Duration: 8 * time.Second, TimeDelta: 62500 * time.Microsecond}
			frame := func(n int) time.Duration { return tc.ss + time.Duration(n)*time.Second*1001/24000 }
			forced, cut := video.FirstForced(tc.ss, 8*time.Second), tc.first
			var cuts []time.Duration
			for n := 0; len(cuts) < tc.cuts; n++ {
				pts := frame(n)
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
			for _, body := range posts(t, tc.shape) {
				lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
				if last := lines[len(lines)-1]; strings.HasPrefix(last, "#") {
					_, start, _, _ := times(t, last)
					want = append(want, start)
				}
			}
			require.GreaterOrEqual(t, len(want), tc.cuts)
			for i := range tc.cuts {
				require.InDelta(t, want[i].Seconds(), cuts[i].Seconds(), 0.014, "segment %d's start", tc.first+i+1)
			}
		})
	}
}

func TestARestartForcesKeyframesOnTheSourcesGrid(t *testing.T) {
	const every = 8 * time.Second
	start := 1800008344 * time.Microsecond // the restart's first frame
	forced := video.FirstForced(start, every)
	require.Equal(t, 225, forced)
	require.True(t, video.KeyframeDue(start, forced, every), "its first frame is a keyframe")
	require.False(t, video.KeyframeDue(start+41*time.Millisecond, forced+1, every), "and the next is not")
	require.True(t, video.KeyframeDue(1808*time.Second+16*time.Millisecond, forced+1, every))
}
