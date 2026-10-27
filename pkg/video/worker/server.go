package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/pipeline"
	"github.com/mediactl/clusterplex/proto/videopb"
)

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
}

func (s *Server) log() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

// Ready is /readyz: the device measured, and a slot free.
func (s *Server) Ready() bool {
	return (s.Measured == nil || s.Measured()) && s.Slots.Free() > 0
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
	err = s.Run(ctx, job, o, out)
	if s.Speed != nil && out.covered > 0 {
		s.Speed(out.covered.Seconds() / max(time.Since(began).Seconds(), 0.001))
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
	mu     sync.Mutex
	r      map[int]*io.PipeReader
	w      map[int]*io.PipeWriter
	stall  time.Duration
	last   atomic.Int64
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
			if _, err := w.Write(c.GetData()); err != nil {
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

// watch fails the job when an open input has sent nothing for stall.
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
			if open > 0 && time.Since(time.Unix(0, in.last.Load())) > in.stall {
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

// sink sends a run's output on the stream. Only the run's goroutine sends.
type sink struct {
	stream  videopb.Video_ConvertServer
	covered time.Duration // the media time the segments sent cover
}

func (s *sink) Segment(seg pipeline.Segment) error {
	s.covered += seg.End - seg.Start
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
	return s.stream.Send(&videopb.Down{Kind: &videopb.Down_Segment{Segment: p}})
}

func (s *sink) File(name string, data []byte) error {
	return s.stream.Send(&videopb.Down{Kind: &videopb.Down_File{File: &videopb.File{Name: name, Data: data}}})
}

func (s *sink) Manifest(mpd []byte) error {
	return s.stream.Send(&videopb.Down{Kind: &videopb.Down_Manifest{Manifest: &videopb.Manifest{Mpd: mpd}}})
}

func (s *sink) Write(off int64, p []byte) error {
	return s.stream.Send(&videopb.Down{Kind: &videopb.Down_Write{Write: &videopb.Write{Offset: off, Data: p}}})
}

func (s *sink) Progress(path, query string) {
	_ = s.stream.Send(&videopb.Down{Kind: &videopb.Down_Progress{Progress: &videopb.Progress{Path: path, Query: query}}})
}
