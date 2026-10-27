package worker_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/pipeline"
	"github.com/mediactl/clusterplex/pkg/video/worker"
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

func TestReadyFollowsTheDeviceAndTheSlots(t *testing.T) {
	measured := false
	s := &worker.Server{Slots: worker.NewSlots(1), Run: blocking, Measured: func() bool { return measured }}
	assert.False(t, s.Ready(), "an unmeasured device takes no work")
	measured = true
	assert.True(t, s.Ready())
	c := serve(t, s)
	first, err := open(t, c, job("A", false))
	require.NoError(t, err)
	defer func() { _ = first.CloseSend() }()
	assert.False(t, s.Ready(), "a full pod is not ready")
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
