// Package relay is the manager's side of the GPU pool: it hands a video
// conversion to a pool pod with a free slot, streams the pod the inputs
// Plex serves, writes each segment the pod streams back into Plex's
// session directory, and only then tells Plex about it. A conversion the
// pool cannot take is refused with its reason: none ever runs on Plex's
// node. It links no ffgo.
package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/mediactl/clusterplex/pkg/hashring"
	"github.com/mediactl/clusterplex/pkg/plexsession"
	"github.com/mediactl/clusterplex/pkg/remoteexec"
	"github.com/mediactl/clusterplex/pkg/video"
	pb "github.com/mediactl/clusterplex/proto"
	"github.com/mediactl/clusterplex/proto/videopb"
)

// hlsName is the only segment name a worker may write for an HLS job:
// Plex's pattern; dashName a DASH job's, the remux pool's names.
var (
	hlsName  = regexp.MustCompile(`^media-\d{5,}\.ts$`)
	dashName = regexp.MustCompile(`^(init-stream[01]|chunk-stream[01]-\d{5,})\.m4s$`)
)

// inputChunk is how much of a streamed input one Chunk carries.
const inputChunk = 256 << 10

// Relay routes Plex's video conversions to the pool.
type Relay struct {
	// Workers are the pool's ready pods.
	Workers remoteexec.WorkerLister
	Dial    func(ctx context.Context, addr string) (videopb.VideoClient, io.Closer, error)
	PMS     plexsession.PMS
	// Live is Plex as its own transcoder reaches it: on loopback inside
	// Plex's network namespace, the only requests Plex serves a Live TV
	// session to; from PMS's address it is direct play, which a session
	// that converts refuses. Zero is PMS.
	Live plexsession.PMS
	// Count publishes how many conversions run (pkg/video/lease); nil
	// publishes nothing.
	Count interface{ Begin(context.Context) func() }
	// Refused is told each refusal's reason; Startup each job's start-up
	// overhead: its first segment's arrival less the input time it covers.
	Refused func(video.Reason)
	Startup func(time.Duration)
	Logger  *slog.Logger
	// Now is the clock. Default time.Now.
	Now func() time.Time
	// ThrottlePoll is how often a held conversion asks Plex whether it may
	// go on, as Plex's transcoder PUTs progress in sloth mode. Default
	// DefaultThrottlePoll.
	ThrottlePoll time.Duration
	// LivePoll is how long a Live TV input waits for its playlist to name a
	// new segment. Default DefaultLivePoll.
	LivePoll time.Duration
}

// DefaultThrottlePoll is Plex's own sloth-mode cadence (finding P).
const DefaultThrottlePoll = 500 * time.Millisecond

func (r *Relay) log() *slog.Logger {
	if r.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Logger
}

func (r *Relay) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// refusal is a conversion ended with a reason other than success.
type refusal struct {
	reason video.Reason
	err    error
}

func (e *refusal) Error() string { return string(e.reason) + ": " + e.err.Error() }
func (e *refusal) Unwrap() error { return e.err }

func refuse(reason video.Reason, err error) error { return &refusal{reason: reason, err: err} }

// finish tells the shim how the job ended: exit 0, or 1 with
// "video: <reason>: <detail>" on stderr.
func (r *Relay) finish(sink remoteexec.Sink, job video.Job, err error) error {
	if err == nil {
		return remoteexec.Finish(sink, "video", nil)
	}
	var rf *refusal
	if !errors.As(err, &rf) {
		rf = &refusal{reason: video.ReasonWorkerError, err: err}
	}
	if r.Refused != nil {
		r.Refused(rf.reason)
	}
	r.log().Warn("video conversion refused", "reason", rf.reason, "session", job.Session, "error", rf.err)
	return remoteexec.Finish(sink, "video", rf)
}

// Execute runs one conversion, start to end, and always answers the shim.
func (r *Relay) Execute(ctx context.Context, req *pb.ExecRequest, sink remoteexec.Sink) error {
	began := r.now()
	job, err := video.Parse(req.GetArgs(), req.GetEnv())
	if err != nil {
		return r.finish(sink, job, refuse(video.ReasonShape, err))
	}
	return r.finish(sink, job, r.run(ctx, req.GetCwd(), job, began))
}

func (r *Relay) run(ctx context.Context, cwd string, job video.Job, began time.Time) error {
	ws, err := r.Workers.ListReady(ctx)
	if err != nil {
		return refuse(video.ReasonNoPool, fmt.Errorf("list the pool: %w", err))
	}
	if len(ws) == 0 {
		return refuse(video.ReasonNoPool, errors.New("no ready video transcode pod"))
	}
	c, err := r.open(ctx, ws, job)
	if err != nil {
		return err
	}
	defer c.close()
	if r.Count != nil {
		defer r.Count.Begin(ctx)()
	}
	log := r.log().With("worker", c.worker, "session", job.Session)
	log.Info("video conversion started", "output", job.Output, "width", job.Video.Width, "height", job.Video.Height)
	go c.pump(ctx, r, job)
	err = r.relay(ctx, cwd, job, c, newLister(r.PMS, job), began)
	if ctx.Err() != nil {
		return refuse(video.ReasonCancelled, ctx.Err())
	}
	return err
}

// conversion is one accepted job's stream to its worker.
type conversion struct {
	worker string
	stream videopb.Video_ConvertClient
	cancel context.CancelFunc
	closer io.Closer
	sendMu sync.Mutex
}

func (c *conversion) close() {
	c.cancel()
	_ = c.closer.Close()
}

func (c *conversion) send(up *videopb.Up) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.stream.Send(up)
}

// order is the pool's pods to try: the one the hash ring names for the
// job first (a tie-break, so a file returns where it ran), then the rest.
func order(ws []remoteexec.Worker, key string) []remoteexec.Worker {
	names := make([]string, len(ws))
	for i, w := range ws {
		names[i] = w.Name
	}
	pick := hashring.New(names...).Locate(key)
	out := slices.Clone(ws)
	slices.SortStableFunc(out, func(a, b remoteexec.Worker) int {
		switch {
		case a.Name == pick:
			return -1
		case b.Name == pick:
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// open hands the job to the first pod that accepts it. A full pod is
// passed over; a pod that cannot run the shape refuses for all of them.
func (r *Relay) open(ctx context.Context, ws []remoteexec.Worker, job video.Job) (*conversion, error) {
	last := refuse(video.ReasonWorkerError, errors.New("no pod could be reached"))
	full := false
	for _, w := range order(ws, job.RingKey()) {
		client, closer, err := r.Dial(ctx, w.Addr)
		if err != nil {
			r.log().Warn("dial a video transcode pod", "worker", w.Name, "error", err)
			last = refuse(video.ReasonWorkerError, fmt.Errorf("dial %s: %w", w.Name, err))
			continue
		}
		cctx, cancel := context.WithCancel(ctx)
		c := &conversion{worker: w.Name, cancel: cancel, closer: closer}
		c.stream, err = client.Convert(cctx)
		if err == nil {
			err = c.send(&videopb.Up{Kind: &videopb.Up_Job{Job: job.Proto()}})
		}
		if err == nil {
			md, herr := c.stream.Header()
			if herr == nil && len(md.Get(video.AcceptedHeader)) > 0 {
				return c, nil
			}
			if _, err = c.stream.Recv(); err == nil {
				err = errors.New("output before the job was accepted")
			}
		}
		c.close()
		switch {
		case ctx.Err() != nil:
			return nil, refuse(video.ReasonCancelled, ctx.Err())
		case status.Code(err) == codes.ResourceExhausted:
			full = true
		case status.Code(err) == codes.InvalidArgument:
			return nil, refuse(video.ReasonShape, fmt.Errorf("%s: %s", w.Name, status.Convert(err).Message()))
		default:
			last = refuse(video.ReasonWorkerError, fmt.Errorf("%s: %w", w.Name, err))
		}
	}
	if full {
		return nil, refuse(video.ReasonFull, fmt.Errorf("every ready pod's slots are taken (%d pods)", len(ws)))
	}
	return nil, last
}

// pump streams every input the worker cannot read itself, each from the
// start, and ends the job's input side when all are sent. A failed read is
// sent as the input's error, which fails the job on the worker.
func (c *conversion) pump(ctx context.Context, r *Relay, job video.Job) {
	var wg sync.WaitGroup
	for i, in := range job.Inputs {
		if !in.Streamed {
			continue
		}
		wg.Go(func() {
			if err := c.stream1(ctx, r, job, int32(i), in); err != nil && ctx.Err() == nil {
				r.log().Warn("stream an input to the pool", "session", job.Session, "input", i, "error", err)
				_ = c.send(&videopb.Up{Kind: &videopb.Up_Input{Input: &videopb.Chunk{Input: int32(i), Error: err.Error()}}})
			}
		})
	}
	wg.Wait()
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	_ = c.stream.CloseSend()
}

func (c *conversion) stream1(ctx context.Context, r *Relay, job video.Job, i int32, in video.Input) error {
	if in.Live {
		return c.followLive(ctx, r, job, i, in)
	}
	var (
		rc  io.ReadCloser
		err error
	)
	if u, perr := url.Parse(in.Source); perr == nil && (u.Scheme == "http" || u.Scheme == "https") {
		rc, err = r.PMS.Open(ctx, in.Source, job.Token)
	} else {
		rc, err = os.Open(in.Source)
	}
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
	defer stop()
	defer func() { _ = rc.Close() }()
	buf := make([]byte, inputChunk)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if serr := c.send(&videopb.Up{Kind: &videopb.Up_Input{Input: &videopb.Chunk{Input: i, Data: slices.Clone(buf[:n])}}}); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) {
			return c.send(&videopb.Up{Kind: &videopb.Up_Input{Input: &videopb.Chunk{Input: i, Eof: true}}})
		}
		if err != nil {
			return err
		}
	}
}

// relay applies the worker's output until its Done.
func (r *Relay) relay(ctx context.Context, cwd string, job video.Job, c *conversion, list *lister, began time.Time) error {
	first := true
	// What the log tells of a job: its first output, its first manifest or
	// list line, the first progress report that failed.
	output, posted, progressFailed := false, false, false
	wall := time.Now() // the log's clock, apart from r.Now's
	for {
		down, err := c.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("the worker ended the stream without Done")
			}
			return refuse(video.ReasonWorkerError, err)
		}
		dash := job.Output == video.OutputDASH
		switch k := down.GetKind().(type) {
		case *videopb.Down_File:
			if !output {
				output = true
				r.log().Info("first output from the pool", "session", job.Session, "after", time.Since(wall))
			}
			if !dash {
				return refuse(video.ReasonWorkerError, errors.New("a file for an HLS job"))
			}
			if err := plexsession.WriteFile(cwd, k.File.GetName(), k.File.GetData(), dashName); err != nil {
				return refuse(video.ReasonWorkerError, err)
			}
		case *videopb.Down_Manifest:
			if !dash {
				return refuse(video.ReasonWorkerError, errors.New("a manifest for an HLS job"))
			}
			if first {
				first = false
				if r.Startup != nil {
					r.Startup(r.now().Sub(began) - job.Segments.Duration)
				}
			}
			// Files before the manifest that lists them, as the remux relay does.
			sent := time.Now()
			if err := r.PMS.Send(ctx, http.MethodPost, job.ManifestURL, "", "", bytes.NewReader(k.Manifest.GetMpd()), job.Token); err != nil {
				return refuse(video.ReasonWorkerError, fmt.Errorf("manifest: %w", err))
			}
			if !posted {
				posted = true
				r.log().Info("first manifest posted", "session", job.Session, "after", time.Since(wall), "took", time.Since(sent))
			}
		case *videopb.Down_Segment:
			if dash {
				return refuse(video.ReasonWorkerError, errors.New("an HLS segment for a DASH job"))
			}
			s := k.Segment
			if !output {
				output = true
				r.log().Info("first output from the pool", "session", job.Session, "after", time.Since(wall))
			}
			if err := plexsession.WriteFile(cwd, s.GetName(), s.GetData(), hlsName); err != nil {
				return refuse(video.ReasonWorkerError, err)
			}
			times := segmentTimes(s)
			if first {
				first = false
				if r.Startup != nil {
					r.Startup(r.now().Sub(began) - (times.End - times.Start))
				}
			}
			var next *time.Duration
			if s.NextStartUs != nil {
				d := time.Duration(s.GetNextStartUs()) * time.Microsecond
				next = &d
			}
			sent := time.Now()
			if err := list.Finished(ctx, times, next); err != nil {
				return refuse(video.ReasonWorkerError, fmt.Errorf("segment list: %w", err))
			}
			if !posted {
				posted = true
				r.log().Info("first segment listed", "session", job.Session, "after", time.Since(wall), "took", time.Since(sent))
			}
		case *videopb.Down_Progress:
			p := k.Progress
			if !progressAllowed(p.GetPath(), p.GetQuery()) {
				r.log().Warn("dropped a progress report outside Plex's", "path", p.GetPath())
				continue
			}
			// Plex's own transcoder carries on past a failed progress PUT too.
			answer, err := r.PMS.Ask(ctx, http.MethodPut, job.ProgressURL, p.GetPath(), p.GetQuery(), http.NoBody, job.Token)
			if err != nil && !progressFailed {
				progressFailed = true
				r.log().Warn("a progress report failed", "session", job.Session, "path", p.GetPath(), "error", err)
			}
			if err == nil && p.GetPath() == "" && canThrottle(answer) {
				// Held here, the relay reads nothing more from the worker,
				// whose bounded windows then stop its pipeline (finding P).
				held := time.Now()
				r.log().Info("Plex throttles the conversion", "session", job.Session, "after", held.Sub(wall), "progress", p.GetQuery())
				if err := r.sloth(ctx, job, p.GetQuery()); err != nil {
					return refuse(video.ReasonCancelled, err)
				}
				r.log().Info("Plex released the conversion", "session", job.Session, "held", time.Since(held))
			}
		case *videopb.Down_Done:
			if msg := k.Done.GetError(); msg != "" {
				reason := video.Reason(k.Done.GetReason())
				if reason == "" {
					reason = video.ReasonWorkerError
				}
				return refuse(reason, errors.New(msg))
			}
			return nil
		default:
			return refuse(video.ReasonWorkerError, fmt.Errorf("unexpected %T", k))
		}
	}
}

func segmentTimes(s *videopb.Segment) video.SegmentTimes {
	t := video.SegmentTimes{
		Name:  s.GetName(),
		Start: time.Duration(s.GetStartUs()) * time.Microsecond,
		End:   time.Duration(s.GetEndUs()) * time.Microsecond,
	}
	for _, sp := range s.GetStreams() {
		t.Streams = append(t.Streams, video.Span{
			Stream: int(sp.GetStream()),
			Start:  time.Duration(sp.GetStartUs()) * time.Microsecond,
			End:    time.Duration(sp.GetEndUs()) * time.Microsecond,
		})
	}
	return t
}

// maxSegmentBytes bounds one Down event: a whole segment.
const maxSegmentBytes = 256 << 20

// DialOptions are the relay's gRPC options toward a pool pod: plaintext
// inside the cluster; fixed windows, so a relay that stops reading holds
// the worker's pipeline within about a segment (finding P; a fixed window
// also turns off gRPC's growing ones); and a ping every 10 s of silence
// during a conversion, so a pod whose node lost power ends its job within
// 20 s rather than leave Plex's transcoder waiting on TCP.
func DialOptions() []grpc.DialOption {
	return dialOptions(keepalive.ClientParameters{Time: 10 * time.Second, Timeout: 10 * time.Second})
}

func dialOptions(ka keepalive.ClientParameters) []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithInitialWindowSize(1 << 20), grpc.WithInitialConnWindowSize(1 << 22),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxSegmentBytes)),
		grpc.WithKeepaliveParams(ka),
	}
}

// GRPCDialer dials a pool pod with DialOptions.
func GRPCDialer(_ context.Context, addr string) (videopb.VideoClient, io.Closer, error) {
	conn, err := grpc.NewClient(addr, DialOptions()...)
	if err != nil {
		return nil, nil, err
	}
	return videopb.NewVideoClient(conn), conn, nil
}
