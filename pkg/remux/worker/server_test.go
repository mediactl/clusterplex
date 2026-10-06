package worker

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/mediactl/clusterplex/pkg/remux"
	"github.com/mediactl/clusterplex/pkg/remux/cache"
	"github.com/mediactl/clusterplex/pkg/remux/pipeline"
	"github.com/mediactl/clusterplex/proto/remuxpb"
)

type stream struct {
	grpc.ServerStream
	ctx    context.Context
	events []*remuxpb.Event
}

func (s *stream) Context() context.Context { return s.ctx }
func (s *stream) Send(e *remuxpb.Event) error {
	s.events = append(s.events, e)
	return nil
}

// fakeRun emits segments from o.From to 4, each 5 s long, and counts runs.
type fakeRun struct{ runs, decoded int }

func (f *fakeRun) Run(_ context.Context, _ remux.Job, o pipeline.Options, sink pipeline.Sink) (int, error) {
	f.runs++
	if err := sink.Init([]byte("vinit"), []byte("ainit"), [2]int32{12288, 48000}, remux.StreamInfo{VideoCodec: "hev1", AudioCodec: "mp4a.40.2", Channels: 2}); err != nil {
		return 0, err
	}
	for n := o.From; n <= 4; n++ {
		f.decoded++
		start := time.Duration(n-1) * 5 * time.Second
		if err := sink.Segment(remux.Segment{
			N: n, Start: start, End: start + 5*time.Second,
			Video: remux.Fragment{Data: []byte{'v', byte(n)}, T: int64(n-1) * 61440, D: 61440},
			Audio: remux.Fragment{Data: []byte{'a', byte(n)}, T: int64(n-1) * 240000, D: 240000},
		}); err != nil {
			return 0, err
		}
	}
	return 4, nil
}

func input(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "a.mkv")
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	return p
}

func files(s *stream) []string {
	var out []string
	for _, e := range s.events {
		switch k := e.Kind.(type) {
		case *remuxpb.Event_File:
			out = append(out, k.File.Name)
		case *remuxpb.Event_Manifest:
			out = append(out, "manifest")
		case *remuxpb.Event_Done:
			out = append(out, "done:"+k.Done.Error)
		}
	}
	return out
}

func TestAFreshJobRunsThePipelineAndEveryFileComesBeforeItsManifest(t *testing.T) {
	f := &fakeRun{}
	srv := &Server{Cache: &cache.Cache{Dir: t.TempDir()}, Run: f.Run}
	s := &stream{ctx: t.Context()}
	j := remux.Job{Input: input(t), SkipToSegment: 1, SegmentDuration: 5 * time.Second, AudioStream: 1, AudioChannels: 2}
	require.NoError(t, srv.Remux(j.Proto(), s))
	assert.Equal(t, []string{
		"init-stream0.m4s", "init-stream1.m4s",
		"chunk-stream0-00001.m4s", "chunk-stream1-00001.m4s", "manifest",
		"chunk-stream0-00002.m4s", "chunk-stream1-00002.m4s", "manifest",
		"chunk-stream0-00003.m4s", "chunk-stream1-00003.m4s", "manifest",
		"chunk-stream0-00004.m4s", "chunk-stream1-00004.m4s", "manifest",
		"manifest", "done:",
	}, files(s))
	last := s.events[len(s.events)-2].GetManifest().GetMpd()
	assert.Contains(t, string(last), `type="static"`)
}

func TestAReplayIsServedFromTheCacheWithoutDecoding(t *testing.T) {
	f := &fakeRun{}
	srv := &Server{Cache: &cache.Cache{Dir: t.TempDir()}, Run: f.Run}
	j := remux.Job{Input: input(t), SkipToSegment: 1, SegmentDuration: 5 * time.Second, AudioStream: 1, AudioChannels: 2}
	first := &stream{ctx: t.Context()}
	require.NoError(t, srv.Remux(j.Proto(), first))
	s := &stream{ctx: t.Context()}
	require.NoError(t, srv.Remux(j.Proto(), s))
	assert.Equal(t, 1, f.runs, "the second play never runs the pipeline")
	assert.Equal(t, files(first), files(s), "the same files and manifests, in the same order")
	assert.True(t, strings.HasPrefix(files(s)[len(files(s))-1], "done:"))
}

func TestAGapIsFilledFromTheEndOfTheLastCachedSegment(t *testing.T) {
	var got pipeline.Options
	f := &fakeRun{}
	srv := &Server{Cache: &cache.Cache{Dir: t.TempDir()}, Run: func(ctx context.Context, j remux.Job, o pipeline.Options, sink pipeline.Sink) (int, error) {
		got = o
		return f.Run(ctx, j, o, sink)
	}}
	in := input(t)
	j := remux.Job{Input: in, SkipToSegment: 1, SegmentDuration: 5 * time.Second, AudioStream: 1, AudioChannels: 2}
	e, err := srv.Cache.Open(srv.key(j, mustStat(t, in)))
	require.NoError(t, err)
	require.NoError(t, e.SetInit([]byte("vinit"), []byte("ainit"), [2]int32{12288, 48000}, remux.StreamInfo{}))
	require.NoError(t, e.Put(remux.Segment{N: 1, End: 5 * time.Second, Video: remux.Fragment{Data: []byte("v1")}, Audio: remux.Fragment{Data: []byte("a1")}}))
	require.NoError(t, srv.Remux(j.Proto(), &stream{ctx: t.Context()}))
	assert.Equal(t, pipeline.Options{From: 2, StartAt: 5 * time.Second}, got)
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	require.NoError(t, err)
	return fi
}

// The e2e test reads the pool's log to tell a replay from a remux.
func TestAReplayLogsThatTheCacheServedIt(t *testing.T) {
	var log bytes.Buffer
	f := &fakeRun{}
	srv := &Server{Cache: &cache.Cache{Dir: t.TempDir()}, Run: f.Run, Logger: slog.New(slog.NewTextHandler(&log, nil))}
	j := remux.Job{Input: input(t), SkipToSegment: 1, SegmentDuration: 5 * time.Second, AudioStream: 1, AudioChannels: 2}
	require.NoError(t, srv.Remux(j.Proto(), &stream{ctx: t.Context()}))
	assert.NotContains(t, log.String(), "served from cache")
	require.NoError(t, srv.Remux(j.Proto(), &stream{ctx: t.Context()}))
	assert.Contains(t, log.String(), "remux served from cache")
}

// overlapStream fails if Send is entered while another Send is running, as
// grpc-go forbids on one stream.
type overlapStream struct {
	stream
	in      atomic.Int32
	overlap atomic.Bool
}

func (s *overlapStream) Send(e *remuxpb.Event) error {
	if s.in.Add(1) > 1 {
		s.overlap.Store(true)
	}
	time.Sleep(time.Millisecond)
	s.in.Add(-1)
	return nil
}

// Review finding I8: the pipeline reports progress from its demuxer and
// segments from its audio side, on different goroutines.
func TestTheStreamIsNeverSentToConcurrently(t *testing.T) {
	run := func(_ context.Context, _ remux.Job, o pipeline.Options, sink pipeline.Sink) (int, error) {
		if err := sink.Init([]byte("v"), []byte("a"), [2]int32{12288, 48000}, remux.StreamInfo{}); err != nil {
			return 0, err
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 50 {
				sink.Progress("", "progress=1.0")
			}
		}()
		go func() {
			defer wg.Done()
			for n := o.From; n < o.From+50; n++ {
				_ = sink.Segment(remux.Segment{N: n, Start: time.Duration(n-1) * 5 * time.Second, End: time.Duration(n) * 5 * time.Second})
			}
		}()
		wg.Wait()
		return o.From + 49, nil
	}
	srv := &Server{Cache: &cache.Cache{Dir: t.TempDir()}, Run: run}
	s := &overlapStream{stream: stream{ctx: t.Context()}}
	j := remux.Job{Input: input(t), SkipToSegment: 1, SegmentDuration: 5 * time.Second}
	require.NoError(t, srv.Remux(j.Proto(), s))
	assert.False(t, s.overlap.Load(), "two Sends overlapped")
}

// Review finding I10: a full cache stops caching; it never fails playback.
func TestAFullCacheStillPlays(t *testing.T) {
	f := &fakeRun{}
	srv := &Server{Cache: &cache.Cache{Dir: t.TempDir(), MaxBytes: 12}, Run: f.Run} // the inits fit, no segment does
	s := &stream{ctx: t.Context()}
	j := remux.Job{Input: input(t), SkipToSegment: 1, SegmentDuration: 5 * time.Second, AudioStream: 1, AudioChannels: 2}
	require.NoError(t, srv.Remux(j.Proto(), s))
	got := files(s)
	assert.Equal(t, "done:", got[len(got)-1])
	assert.Contains(t, got, "chunk-stream0-00004.m4s")
}
