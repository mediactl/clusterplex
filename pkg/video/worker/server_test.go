package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/mediactl/clusterplex/pkg/plexsession"
	"github.com/mediactl/clusterplex/pkg/remoteexec"
	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/internal/netcut"
	"github.com/mediactl/clusterplex/pkg/video/pipeline"
	"github.com/mediactl/clusterplex/pkg/video/relay"
	"github.com/mediactl/clusterplex/pkg/video/worker"
	pb "github.com/mediactl/clusterplex/proto"
	"github.com/mediactl/clusterplex/proto/videopb"
)

// serve runs s on an in-memory listener and returns a client.
func serve(t *testing.T, s *worker.Server) videopb.VideoClient {
	t.Helper()
	l := bufconn.Listen(16 << 20)
	srv := grpc.NewServer()
	videopb.RegisterVideoServer(srv, s)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///pool", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return videopb.NewVideoClient(conn)
}

func job(session string, streamed bool) *videopb.Job {
	j := video.Job{Output: video.OutputHLS, Session: session, Inputs: []video.Input{{Path: "/media/x.mkv"}}}
	if streamed {
		j.Inputs = []video.Input{{Streamed: true, Format: "mpegts"}}
	}
	return j.Proto()
}

// open sends the job and waits for the worker's answer: accepted, or the
// status it refused with.
func open(t *testing.T, c videopb.VideoClient, j *videopb.Job) (videopb.Video_ConvertClient, error) {
	t.Helper()
	s, err := c.Convert(t.Context())
	require.NoError(t, err)
	require.NoError(t, s.Send(&videopb.Up{Kind: &videopb.Up_Job{Job: j}}))
	md, err := s.Header()
	require.NoError(t, err)
	if len(md.Get(video.AcceptedHeader)) > 0 {
		return s, nil
	}
	_, err = s.Recv()
	return nil, err
}

// last reads the stream to its end and returns its Done.
func last(t *testing.T, s videopb.Video_ConvertClient) *videopb.Done {
	t.Helper()
	var d *videopb.Done
	for {
		m, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return d
		}
		require.NoError(t, err)
		if m.GetDone() != nil {
			require.Nil(t, d, "a second Done")
			d = m.GetDone()
		}
	}
}

func blocking(ctx context.Context, _ video.Job, _ pipeline.Options, _ pipeline.Sink) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestAJobBeyondTheSlotsIsResourceExhausted(t *testing.T) {
	c := serve(t, &worker.Server{Slots: worker.NewSlots(1), Run: blocking})
	first, err := open(t, c, job("A", false))
	require.NoError(t, err)
	defer func() { _ = first.CloseSend() }()
	_, err = open(t, c, job("B", false))
	require.Equal(t, codes.ResourceExhausted, status.Code(err), "%v", err)
}

func TestStreamedInputChunksReachThePipeline(t *testing.T) {
	var got bytes.Buffer
	c := serve(t, &worker.Server{Slots: worker.NewSlots(1), Run: func(_ context.Context, _ video.Job, o pipeline.Options, _ pipeline.Sink) error {
		_, err := io.Copy(&got, o.Inputs(0))
		return err
	}})
	s, err := open(t, c, job("A", true))
	require.NoError(t, err)
	want := bytes.Repeat([]byte("live"), 200_000)
	for b := want; len(b) > 0; {
		n := min(len(b), 64<<10)
		require.NoError(t, s.Send(&videopb.Up{Kind: &videopb.Up_Input{Input: &videopb.Chunk{Input: 0, Data: b[:n]}}}))
		b = b[n:]
	}
	require.NoError(t, s.Send(&videopb.Up{Kind: &videopb.Up_Input{Input: &videopb.Chunk{Input: 0, Eof: true}}}))
	require.NoError(t, s.CloseSend())
	d := last(t, s)
	require.NotNil(t, d)
	assert.Empty(t, d.GetError())
	assert.True(t, bytes.Equal(want, got.Bytes()), "the pipeline read %d of %d bytes", got.Len(), len(want))
}

func TestEveryConversionEndsWithDone(t *testing.T) {
	for name, tc := range map[string]struct {
		run    error
		reason video.Reason
	}{
		"success":         {nil, ""},
		"a failure":       {errors.New("boom"), video.ReasonWorkerError},
		"a shape refusal": {video.ErrShape, video.ReasonShape},
	} {
		t.Run(name, func(t *testing.T) {
			c := serve(t, &worker.Server{Slots: worker.NewSlots(1), Run: func(context.Context, video.Job, pipeline.Options, pipeline.Sink) error {
				return tc.run
			}})
			s, err := open(t, c, job("A", false))
			require.NoError(t, err)
			require.NoError(t, s.CloseSend())
			d := last(t, s)
			require.NotNil(t, d)
			assert.Equal(t, string(tc.reason), d.GetReason())
			if tc.run != nil {
				assert.Contains(t, d.GetError(), tc.run.Error())
			}
		})
	}
}

func TestAStalledStreamedInputReleasesItsSlot(t *testing.T) {
	slots := worker.NewSlots(1)
	c := serve(t, &worker.Server{Slots: slots, InputStall: 100 * time.Millisecond, Run: func(_ context.Context, _ video.Job, o pipeline.Options, _ pipeline.Sink) error {
		_, err := io.Copy(io.Discard, o.Inputs(0))
		return err
	}})
	s, err := open(t, c, job("A", true))
	require.NoError(t, err)
	require.NoError(t, s.Send(&videopb.Up{Kind: &videopb.Up_Input{Input: &videopb.Chunk{Input: 0, Data: []byte("a little")}}}))
	// ... and then nothing: the upstream stalled, the stream stays open.
	d := last(t, s)
	require.NotNil(t, d)
	assert.Equal(t, string(video.ReasonWorkerError), d.GetReason())
	assert.Contains(t, d.GetError(), "input stalled")
	require.Eventually(t, func() bool { return slots.Free() == 1 }, 2*time.Second, 10*time.Millisecond)
}

func TestReadyFollowsTheDeviceAlone(t *testing.T) {
	// A full pod stays ready: Plex restarts a session at a seek within
	// milliseconds, and only a pod the relay lists can take the restart
	// into its session's slot; a new session it refuses full.
	measured := false
	s := &worker.Server{Slots: worker.NewSlots(1), Run: blocking, Measured: func() bool { return measured }}
	assert.False(t, s.Ready(), "an unmeasured device takes no work")
	measured = true
	assert.True(t, s.Ready())
	c := serve(t, s)
	first, err := open(t, c, job("A", false))
	require.NoError(t, err)
	defer func() { _ = first.CloseSend() }()
	assert.True(t, s.Ready(), "a full pod is still listed")
	seek, err := open(t, c, job("A", false))
	require.NoError(t, err, "the seek takes session A's slot")
	defer func() { _ = seek.CloseSend() }()
	_, err = open(t, c, job("B", false))
	require.Equal(t, codes.ResourceExhausted, status.Code(err), "%v", err)
}

func TestTheEncodeSpeedIsObserved(t *testing.T) {
	var speeds []float64
	c := serve(t, &worker.Server{
		Slots: worker.NewSlots(1), Speed: func(x float64) { speeds = append(speeds, x) },
		Run: func(_ context.Context, _ video.Job, _ pipeline.Options, sink pipeline.Sink) error {
			for i := range 2 {
				start := time.Duration(i) * 8 * time.Second
				if err := sink.Segment(pipeline.Segment{SegmentTimes: video.SegmentTimes{
					Name: video.SegmentName("media-%05d.ts", i), Start: start, End: start + 8*time.Second,
				}}); err != nil {
					return err
				}
			}
			time.Sleep(100 * time.Millisecond)
			return nil
		},
	})
	s, err := open(t, c, job("A", false))
	require.NoError(t, err)
	require.NoError(t, s.CloseSend())
	require.NotNil(t, last(t, s))
	require.Len(t, speeds, 1)
	assert.Greater(t, speeds[0], 10.0, "16 s of media in about 0.1 s")
}

// shimSink is the shim's side of a relayed job: what it was told.
type shimSink struct{}

func (shimSink) Send(*pb.TranscodeLog) error { return nil }

func TestAHeldRelayStallsThePipeline(t *testing.T) {
	// The relay held by Plex's throttle (finding P) reads nothing, and the
	// worker's bounded windows then stop its pipeline in Send: no more than
	// about a segment is made ahead. Released, it carries on to the end.
	const segments = 12
	var made atomic.Int32
	l := bufconn.Listen(64 << 20)
	srv := grpc.NewServer(worker.ServerOptions()...)
	videopb.RegisterVideoServer(srv, &worker.Server{Slots: worker.NewSlots(1), Run: func(_ context.Context, _ video.Job, _ pipeline.Options, sink pipeline.Sink) error {
		sink.Progress("", "progress=1.0&size=-1&remaining=60&speed=2.0&vdec_hw_status=0")
		for i := range segments {
			start := time.Duration(i) * 8 * time.Second
			st := video.SegmentTimes{
				Name: video.SegmentName("media-%05d.ts", i), Start: start, End: start + 8*time.Second,
				Streams: []video.Span{{Stream: 0, Start: start, End: start + 8*time.Second}},
			}
			if err := sink.Segment(pipeline.Segment{SegmentTimes: st, Data: make([]byte, 2<<20)}); err != nil {
				return err
			}
			made.Add(1)
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}})
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)

	var throttled atomic.Bool
	throttled.Store(true)
	var sloth atomic.Bool
	pms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/progress"):
			w.WriteHeader(http.StatusPartialContent)
			if throttled.Load() {
				_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<MediaContainer size="0" canThrottle="1">`+"\n</MediaContainer>\n")
				return
			}
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<MediaContainer size="0">`+"\n</MediaContainer>\n")
		case strings.Contains(r.URL.RawQuery, "sloth"):
			sloth.Store(true)
		}
	}))
	t.Cleanup(pms.Close)
	r := &relay.Relay{
		Workers: readyPool{{Name: "video-0", Addr: "video-0"}},
		Dial: func(context.Context, string) (videopb.VideoClient, io.Closer, error) {
			conn, err := grpc.NewClient("passthrough:///video-0", append(relay.DialOptions(),
				grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }))...)
			if err != nil {
				return nil, nil, err
			}
			return videopb.NewVideoClient(conn), conn, nil
		},
		PMS:          plexsession.PMS{Addr: pms.URL, HTTP: pms.Client()},
		ThrottlePoll: 20 * time.Millisecond,
	}
	c := fixture(t)
	ran := make(chan error, 1)
	go func() {
		ran <- r.Execute(t.Context(), &pb.ExecRequest{Args: c.Args, Env: map[string]string{"X_PLEX_TOKEN": "tok"}, Cwd: t.TempDir()}, shimSink{})
	}()

	require.Eventually(t, sloth.Load, 5*time.Second, 5*time.Millisecond, "the relay never went into sloth mode")
	held := made.Load()
	time.Sleep(300 * time.Millisecond)
	assert.LessOrEqual(t, made.Load()-held, int32(2), "the pipeline ran on while the relay was held")
	assert.Less(t, made.Load(), int32(segments))

	throttled.Store(false)
	select {
	case err := <-ran:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the pipeline did not carry on once released")
	}
	assert.Equal(t, int32(segments), made.Load())
}

func TestAHeldPipelineIsNoStalledInput(t *testing.T) {
	// A pipeline held by the relay reads nothing from its streamed input,
	// which then backs up to the manager: the input did not stall, and the
	// watchdog must not fail the job for it.
	c := serve(t, &worker.Server{Slots: worker.NewSlots(1), InputStall: 100 * time.Millisecond, Run: func(_ context.Context, _ video.Job, o pipeline.Options, _ pipeline.Sink) error {
		in := o.Inputs(0)
		if _, err := io.ReadFull(in, make([]byte, 1024)); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond) // held
		_, err := io.Copy(io.Discard, in)
		return err
	}})
	s, err := open(t, c, job("A", true))
	require.NoError(t, err)
	go func() {
		chunk := bytes.Repeat([]byte("live"), 16<<10)
		for range 64 {
			if s.Send(&videopb.Up{Kind: &videopb.Up_Input{Input: &videopb.Chunk{Input: 0, Data: chunk}}}) != nil {
				return
			}
		}
		_ = s.Send(&videopb.Up{Kind: &videopb.Up_Input{Input: &videopb.Chunk{Input: 0, Eof: true}}})
		_ = s.CloseSend()
	}()
	d := last(t, s)
	require.NotNil(t, d)
	assert.Empty(t, d.GetError())
}

type readyPool []remoteexec.Worker

func (p readyPool) ListReady(context.Context) ([]remoteexec.Worker, error) { return p, nil }

// fixture is the captured library HLS job (pkg/video's testdata).
func fixture(t *testing.T) struct{ Args []string } {
	t.Helper()
	b, err := os.ReadFile("../testdata/plex-video-jobs.jsonl")
	require.NoError(t, err)
	for _, line := range strings.Split(string(b), "\n") {
		var c struct {
			Shape string   `json:"shape"`
			Args  []string `json:"args"`
		}
		if json.Unmarshal([]byte(line), &c) == nil && c.Shape == "library-hls" {
			return struct{ Args []string }{c.Args}
		}
	}
	t.Fatal("no library-hls fixture")
	return struct{ Args []string }{}
}

// openCtx is open on a context of its own, which the caller cancels.
func openCtx(t *testing.T, ctx context.Context, c videopb.VideoClient, j *videopb.Job) error {
	t.Helper()
	s, err := c.Convert(ctx)
	require.NoError(t, err)
	require.NoError(t, s.Send(&videopb.Up{Kind: &videopb.Up_Job{Job: j}}))
	md, err := s.Header()
	if err != nil {
		return err
	}
	if len(md.Get(video.AcceptedHeader)) > 0 {
		return nil
	}
	_, err = s.Recv()
	return err
}

func TestAConversionThatOutlivesItsContextIsAbandoned(t *testing.T) {
	// An ffgo call that hangs (a QSV encode the iHD driver never returns)
	// must not hold its slot for good: once its stream ends and it has not
	// returned within the grace, its slot is freed and it is counted, and
	// at MaxAbandoned the pod reports itself wedged, so /healthz fails and
	// the kubelet restarts it, as clustarr's pkg/ffruntime does.
	unblock := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	t.Cleanup(release)
	var sent atomic.Int32
	hang := func(_ context.Context, _ video.Job, _ pipeline.Options, sink pipeline.Sink) error {
		<-unblock
		if sink.Segment(pipeline.Segment{SegmentTimes: video.SegmentTimes{Name: "media-00000.ts", End: time.Second}}) == nil {
			sent.Add(1)
		}
		return nil
	}
	slots := worker.NewSlots(2)
	s := &worker.Server{Slots: slots, Run: hang, Grace: 50 * time.Millisecond, MaxAbandoned: 2}
	c := serve(t, s)
	for i, session := range []string{"A", "B"} {
		ctx, cancel := context.WithCancel(t.Context())
		require.NoError(t, openCtx(t, ctx, c, job(session, false)))
		require.False(t, s.Wedged(), "after %d abandoned", i)
		cancel()
		require.Eventually(t, func() bool { return slots.Free() == 2 }, 2*time.Second, 10*time.Millisecond,
			"session %s's slot is freed though its conversion never returned", session)
	}
	require.True(t, s.Wedged(), "two abandoned conversions wedge the pod")
	err := openCtx(t, t.Context(), c, job("C", false))
	require.Equal(t, codes.Unavailable, status.Code(err), "a wedged pod takes no work: %v", err)
	release()
	require.Eventually(t, func() bool { return s.Abandoned() == 0 }, 2*time.Second, 10*time.Millisecond)
	assert.Zero(t, sent.Load(), "an abandoned conversion sends nothing on its ended stream")
	assert.True(t, s.Wedged(), "a wedged pod stays wedged until it restarts")
}

func TestARelayThatGoesSilentFreesItsSlot(t *testing.T) {
	// Plex's node losing power resets no connection: without keepalive the
	// conversion's sends would block until TCP gave up, about 15 minutes,
	// its slot taken meanwhile.
	slots := worker.NewSlots(1)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer(worker.ServerOptionsWith(keepalive.ServerParameters{Time: time.Second, Timeout: time.Second})...)
	videopb.RegisterVideoServer(srv, &worker.Server{Slots: slots, Run: blocking})
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	p, err := netcut.New(l.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	conn, err := grpc.NewClient(p.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = open(t, videopb.NewVideoClient(conn), job("A", false))
	require.NoError(t, err)
	require.Zero(t, slots.Free())

	p.Cut()
	require.Eventually(t, func() bool { return slots.Free() == 1 }, 10*time.Second, 50*time.Millisecond,
		"the silent relay's conversion ends and frees its slot")
}
