package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/pipeline"
	"github.com/mediactl/clusterplex/proto/videopb"
)

// ServerOptions are the pool's gRPC server options: fixed windows for what
// the relay streams in, small messages in and whole segments out, and
// keepalive: Plex's node losing power resets no connection, and a
// conversion's sends would block until TCP gave up, about 15 minutes, its
// slot taken meanwhile. The relay pings every 10 s (DialOptions), which the
// enforcement policy admits.
func ServerOptions() []grpc.ServerOption {
	return serverOptions(keepalive.ServerParameters{Time: 10 * time.Second, Timeout: 10 * time.Second})
}

func serverOptions(ka keepalive.ServerParameters) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.InitialWindowSize(1 << 20), grpc.InitialConnWindowSize(1 << 22),
		grpc.MaxRecvMsgSize(1 << 20), grpc.MaxSendMsgSize(256 << 20),
		grpc.KeepaliveParams(ka),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second}),
	}
}

// DefaultInputStall is how long a streamed input may send nothing before
// its job fails: a Live TV upstream that stalls must not hold a slot.
const DefaultInputStall = 30 * time.Second

var errStalled = errors.New("input stalled")

// Server is the pool's Video service: one conversion per slot.
type Server struct {
	videopb.UnimplementedVideoServer
	Slots   *Slots
	Run     func(ctx context.Context, job video.Job, o pipeline.Options, sink pipeline.Sink) error
	Options pipeline.Options
	// InputStall is DefaultInputStall when zero.
	InputStall time.Duration
	// Measured reports the device proved itself; nil is true.
	Measured func() bool
	// Active is told each conversion's start (+1) and end (-1) by kind.
	Active func(kind video.OutputKind, delta int)
	// Speed is told each finished conversion's speed: the media time its
	// segments cover per second it ran.
	Speed  func(x float64)
	Logger *slog.Logger
	// Grace is how long a conversion may run on once its context ends
	// before it is abandoned: its slot freed, the call left running and
	// counted until it returns. DefaultGrace when zero.
	Grace time.Duration
	// MaxAbandoned abandoned conversions still running wedge the pod for
	// good: it takes no work and Wedged reports it, so /healthz fails and
	// the kubelet restarts it. DefaultMaxAbandoned when zero.
	MaxAbandoned int

	abandonMu sync.Mutex
	abandoned int
	wedged    bool
}

// DefaultGrace and DefaultMaxAbandoned bound ffgo's calls as clustarr's
// pkg/ffruntime does: a QSV call the iHD driver never returns must not hold
// a slot, nor leave a pod that can no longer convert passing its probes.
const (
	DefaultGrace        = 5 * time.Second
	DefaultMaxAbandoned = 4
)

var errAbandoned = errors.New("the conversion outlived its stream and was abandoned")

// Wedged reports MaxAbandoned abandoned conversions; it never clears.
func (s *Server) Wedged() bool {
	s.abandonMu.Lock()
	defer s.abandonMu.Unlock()
	return s.wedged
}

// Abandoned is how many abandoned conversions are still running.
func (s *Server) Abandoned() int {
	s.abandonMu.Lock()
	defer s.abandonMu.Unlock()
	return s.abandoned
}

func (s *Server) log() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

// Ready is /readyz: the device measured. A full pod stays ready: a seek's
// restart takes its session's slot, and the relay passes over a pod that
// answers a new session ResourceExhausted.
func (s *Server) Ready() bool {
	return s.Measured == nil || s.Measured()
}

// Convert runs one job: a slot, the header that accepts it, its streamed
// inputs piped into the pipeline, its output sent back, and one Done.
func (s *Server) Convert(stream videopb.Video_ConvertServer) error {
	up, err := stream.Recv()
	if err != nil {
		return err
	}
	if up.GetJob() == nil {
		return status.Error(codes.InvalidArgument, "the first message is not a job")
	}
	job := video.JobFromProto(up.GetJob())
	if s.Measured != nil && !s.Measured() {
		return status.Error(codes.Unavailable, "the device has not proved itself")
	}
	if s.Wedged() {
		return status.Error(codes.Unavailable, "the pod is wedged: its abandoned conversions still run")
	}
	ctx, cancel := context.WithCancelCause(stream.Context())
	defer cancel(nil)
	release, ok := s.Slots.Acquire(job.Session, job.Output, func() { cancel(context.Canceled) })
	if !ok {
		return status.Error(codes.ResourceExhausted, "every slot is taken")
	}
	defer release()
	if err := stream.SendHeader(metadata.Pairs(video.AcceptedHeader, "1")); err != nil {
		return err
	}
	if s.Active != nil {
		s.Active(job.Output, 1)
		defer s.Active(job.Output, -1)
	}
	log := s.log().With("session", job.Session, "output", job.Output)
	log.Info("conversion started")

	in := newInputs(job, s.inputStall(), cancel)
	go in.receive(stream)
	defer in.close()
	o := s.Options
	o.Inputs = in.reader
	out := &sink{stream: stream}
	began := time.Now()
	err = s.run(ctx, job, o, out)
	if errors.Is(err, errAbandoned) {
		// The call may still be sending: no Done; the stream ends with this.
		log.Error("conversion abandoned", "error", err, "abandoned", s.Abandoned())
		return status.Error(codes.Aborted, err.Error())
	}
	if c := time.Duration(out.covered.Load()); s.Speed != nil && c > 0 {
		s.Speed(c.Seconds() / max(time.Since(began).Seconds(), 0.001))
	}
	if cause := context.Cause(ctx); err != nil && cause != nil && !errors.Is(cause, context.Canceled) {
		err = cause
	}
	done := &videopb.Done{}
	if err != nil {
		done.Error, done.Reason = err.Error(), string(reasonOf(ctx, err))
		log.Warn("conversion failed", "reason", done.Reason, "error", err)
	} else {
		log.Info("conversion done")
	}
	_ = stream.Send(&videopb.Down{Kind: &videopb.Down_Done{Done: done}})
	return nil
}

// run runs the pipeline bounded by ctx: once ctx ends, it has Grace to
// return, or it is abandoned -- left running with its sink closed, and
// counted until it returns.
func (s *Server) run(ctx context.Context, job video.Job, o pipeline.Options, out *sink) error {
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, job, o, out) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	t := time.NewTimer(s.grace())
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
	}
	out.closed.Store(true)
	s.abandonMu.Lock()
	s.abandoned++
	if s.abandoned >= s.maxAbandoned() {
		s.wedged = true
	}
	s.abandonMu.Unlock()
	go func() {
		<-done
		s.abandonMu.Lock()
		s.abandoned--
		s.abandonMu.Unlock()
	}()
	return fmt.Errorf("%w: %w", errAbandoned, context.Cause(ctx))
}

func (s *Server) grace() time.Duration {
	if s.Grace <= 0 {
		return DefaultGrace
	}
	return s.Grace
}

func (s *Server) maxAbandoned() int {
	if s.MaxAbandoned <= 0 {
		return DefaultMaxAbandoned
	}
	return s.MaxAbandoned
}

func (s *Server) inputStall() time.Duration {
	if s.InputStall <= 0 {
		return DefaultInputStall
	}
	return s.InputStall
}

func reasonOf(ctx context.Context, err error) video.Reason {
	switch {
	case errors.Is(err, video.ErrShape):
		return video.ReasonShape
	case errors.Is(err, errStalled):
		return video.ReasonWorkerError
	case ctx.Err() != nil:
		return video.ReasonCancelled
	default:
		return video.ReasonWorkerError
	}
}

// inputs are a job's streamed inputs: a pipe each, filled from the
// stream's Chunks, which the pipeline reads; a watchdog fails the job when
// an open input sends nothing for stall.
type inputs struct {
	mu    sync.Mutex
	r     map[int]*io.PipeReader
	w     map[int]*io.PipeWriter
	stall time.Duration
	last  atomic.Int64
	// held is set while a chunk waits for the pipeline to read it: the
	// pipeline holds the input then (a throttled relay), not the upstream.
	held   atomic.Bool
	cancel context.CancelCauseFunc
	done   chan struct{}
}

func newInputs(job video.Job, stall time.Duration, cancel context.CancelCauseFunc) *inputs {
	in := &inputs{r: map[int]*io.PipeReader{}, w: map[int]*io.PipeWriter{}, stall: stall, cancel: cancel, done: make(chan struct{})}
	for i, src := range job.Inputs {
		if src.Streamed {
			in.r[i], in.w[i] = io.Pipe()
		}
	}
	in.last.Store(time.Now().UnixNano())
	if len(in.w) > 0 {
		go in.watch()
	}
	return in
}

func (in *inputs) reader(i int) io.Reader { return in.r[i] }

// receive pipes each Chunk to its input until the stream ends. A write
// blocks while the pipeline does not read, which holds the relay too.
func (in *inputs) receive(stream videopb.Video_ConvertServer) {
	for {
		up, err := stream.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				in.fail(err)
			}
			return
		}
		c := up.GetInput()
		if c == nil {
			continue
		}
		in.last.Store(time.Now().UnixNano())
		in.mu.Lock()
		w := in.w[int(c.GetInput())]
		in.mu.Unlock()
		if w == nil {
			continue
		}
		switch {
		case c.GetError() != "":
			in.end(int(c.GetInput()), errors.New(c.GetError()))
		case len(c.GetData()) > 0:
			in.held.Store(true)
			_, err := w.Write(c.GetData())
			in.held.Store(false)
			in.last.Store(time.Now().UnixNano())
			if err != nil {
				return
			}
		}
		if c.GetEof() {
			in.end(int(c.GetInput()), nil)
		}
	}
}

// end closes input i, with err or at EOF.
func (in *inputs) end(i int, err error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if w := in.w[i]; w != nil {
		_ = w.CloseWithError(err)
		delete(in.w, i)
	}
}

func (in *inputs) fail(err error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for i, w := range in.w {
		_ = w.CloseWithError(err)
		delete(in.w, i)
	}
}

// watch fails the job when an open input has sent nothing for stall, the
// time a chunk waited on the pipeline aside.
func (in *inputs) watch() {
	t := time.NewTicker(max(in.stall/4, 10*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-in.done:
			return
		case <-t.C:
			in.mu.Lock()
			open := len(in.w)
			in.mu.Unlock()
			if open > 0 && !in.held.Load() && time.Since(time.Unix(0, in.last.Load())) > in.stall {
				in.cancel(errStalled)
				in.fail(errStalled)
				return
			}
		}
	}
}

func (in *inputs) close() {
	close(in.done)
	in.fail(io.ErrClosedPipe)
	for _, r := range in.r {
		_ = r.Close()
	}
}

// sink sends a run's output on the stream. Only the run's goroutine sends;
// once the run is abandoned, nothing is sent.
type sink struct {
	stream  videopb.Video_ConvertServer
	covered atomic.Int64 // the media time the segments sent cover
	closed  atomic.Bool
}

func (s *sink) send(d *videopb.Down) error {
	if s.closed.Load() {
		return errAbandoned
	}
	return s.stream.Send(d)
}

func (s *sink) Segment(seg pipeline.Segment) error {
	if s.closed.Load() {
		return errAbandoned
	}
	s.covered.Add(int64(seg.End - seg.Start))
	p := &videopb.Segment{
		Name: seg.Name, Data: seg.Data, StartUs: seg.Start.Microseconds(), EndUs: seg.End.Microseconds(),
	}
	for _, sp := range seg.Streams {
		p.Streams = append(p.Streams, &videopb.Span{Stream: int32(sp.Stream), StartUs: sp.Start.Microseconds(), EndUs: sp.End.Microseconds()})
	}
	if seg.NextStart != nil {
		n := seg.NextStart.Microseconds()
		p.NextStartUs = &n
	}
	return s.send(&videopb.Down{Kind: &videopb.Down_Segment{Segment: p}})
}

func (s *sink) File(name string, data []byte) error {
	return s.send(&videopb.Down{Kind: &videopb.Down_File{File: &videopb.File{Name: name, Data: data}}})
}

func (s *sink) Manifest(mpd []byte) error {
	return s.send(&videopb.Down{Kind: &videopb.Down_Manifest{Manifest: &videopb.Manifest{Mpd: mpd}}})
}

func (s *sink) Write(off int64, p []byte) error {
	return s.send(&videopb.Down{Kind: &videopb.Down_Write{Write: &videopb.Write{Offset: off, Data: p}}})
}

func (s *sink) Progress(path, query string) {
	_ = s.send(&videopb.Down{Kind: &videopb.Down_Progress{Progress: &videopb.Progress{Path: path, Query: query}}})
}
