package relay_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/mediactl/clusterplex/pkg/plexsession"
	"github.com/mediactl/clusterplex/pkg/remoteexec"
	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/relay"
	pb "github.com/mediactl/clusterplex/proto"
	"github.com/mediactl/clusterplex/proto/videopb"
)

// worker is a scripted pool pod: it refuses with a code, or accepts and
// runs its script while it gathers what the relay streams in.
type worker struct {
	videopb.UnimplementedVideoServer
	refuse codes.Code
	run    func(ctx context.Context, send func(*videopb.Down) error) error

	mu    sync.Mutex
	job   []byte // the Job as it arrived, marshalled
	input bytes.Buffer
	eof   chan struct{}
}

func (w *worker) Convert(stream videopb.Video_ConvertServer) error {
	up, err := stream.Recv()
	if err != nil {
		return err
	}
	b, _ := proto.Marshal(up.GetJob())
	w.mu.Lock()
	w.job = b
	w.mu.Unlock()
	if w.refuse != codes.OK {
		return status.Error(w.refuse, "refused by the script")
	}
	if err := stream.SendHeader(metadata.Pairs(video.AcceptedHeader, "1")); err != nil {
		return err
	}
	go func() {
		for {
			up, err := stream.Recv()
			if err != nil {
				return
			}
			if c := up.GetInput(); c != nil {
				w.mu.Lock()
				w.input.Write(c.GetData())
				w.mu.Unlock()
				if c.GetEof() && w.eof != nil {
					close(w.eof)
				}
			}
		}
	}()
	return w.run(stream.Context(), stream.Send)
}

// pool serves each worker on its own in-memory listener, named by address.
func pool(t *testing.T, ws map[string]*worker) (remoteexec.WorkerLister, func(context.Context, string) (videopb.VideoClient, io.Closer, error)) {
	t.Helper()
	lis := map[string]*bufconn.Listener{}
	var ready workers
	for _, name := range []string{"video-0", "video-1", "video-2"} {
		w, ok := ws[name]
		if !ok {
			continue
		}
		l := bufconn.Listen(16 << 20)
		srv := grpc.NewServer()
		videopb.RegisterVideoServer(srv, w)
		go func() { _ = srv.Serve(l) }()
		t.Cleanup(srv.Stop)
		lis[name] = l
		ready = append(ready, remoteexec.Worker{Name: name, Addr: name})
	}
	dial := func(_ context.Context, addr string) (videopb.VideoClient, io.Closer, error) {
		l := lis[addr]
		conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }))
		if err != nil {
			return nil, nil, err
		}
		return videopb.NewVideoClient(conn), conn, nil
	}
	return ready, dial
}

type workers []remoteexec.Worker

func (w workers) ListReady(context.Context) ([]remoteexec.Worker, error) { return w, nil }

// pms records what reaches Plex: each request, its token, its body, and for
// a segment list which files the session directory held as it was read.
type pms struct {
	mu      sync.Mutex
	dir     string
	serve   map[string][]byte // GET paths it answers, with the session's token
	calls   []string
	tokens  []string
	lists   []string
	present [][]string
	// throttled answers a progress PUT as PMS does while a job is its
	// throttle buffer ahead of the client (finding P).
	throttled atomic.Bool
	// failing answers every progress PUT 500.
	failing atomic.Bool
}

// Plex's answers to a progress PUT, as captured (library-hls-play).
const (
	plainAnswer     = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<MediaContainer size=\"0\">\n</MediaContainer>\n"
	throttledAnswer = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<MediaContainer size=\"0\" canThrottle=\"1\">\n</MediaContainer>\n"
)

func (p *pms) snapshot() (calls, lists []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls), slices.Clone(p.lists)
}

func (p *pms) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	p.tokens = append(p.tokens, r.Header.Get("X-Plex-Token"))
	if r.Method == http.MethodGet {
		if b, ok := p.serve[r.URL.Path]; ok && r.Header.Get("X-Plex-Token") == "tok" {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/progress") {
		if p.failing.Load() {
			http.Error(w, "gone", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/xml;charset=utf-8")
		w.WriteHeader(http.StatusPartialContent)
		if p.throttled.Load() {
			_, _ = io.WriteString(w, throttledAnswer)
		} else {
			_, _ = io.WriteString(w, plainAnswer)
		}
		return
	}
	if strings.HasSuffix(r.URL.Path, "/manifest") {
		p.lists = append(p.lists, string(body))
		ents, _ := os.ReadDir(p.dir)
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		p.present = append(p.present, names)
	}
}

type sink struct {
	mu   sync.Mutex
	logs []*pb.TranscodeLog
}

func (s *sink) Send(m *pb.TranscodeLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, m)
	return nil
}

func (s *sink) stderr() string {
	var b strings.Builder
	for _, m := range s.logs {
		b.Write(m.GetStderrChunk())
	}
	return b.String()
}

func (s *sink) exit() int32 {
	for _, m := range s.logs {
		if m.GetIsFinished() {
			return m.GetExitCode()
		}
	}
	return -1
}

// counter counts the Lease's Begins and their ends.
type counter struct {
	mu          sync.Mutex
	begun, done int
}

func (c *counter) Begin(context.Context) func() {
	c.mu.Lock()
	c.begun++
	c.mu.Unlock()
	return func() { c.mu.Lock(); c.done++; c.mu.Unlock() }
}

// libraryArgs is the captured library HLS job (pkg/video's fixture).
func libraryArgs(t *testing.T) []string {
	t.Helper()
	f, err := os.Open("../testdata/plex-video-jobs.jsonl")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		var c struct {
			Shape string   `json:"shape"`
			Args  []string `json:"args"`
		}
		require.NoError(t, json.Unmarshal(s.Bytes(), &c))
		if c.Shape == "library-hls" {
			return c.Args
		}
	}
	t.Fatal("no library-hls fixture")
	return nil
}

type harness struct {
	relay *relay.Relay
	pms   *pms
	req   *pb.ExecRequest
	count *counter
	refs  []video.Reason
}

func setup(t *testing.T, ws map[string]*worker) *harness {
	t.Helper()
	dir := t.TempDir()
	p := &pms{dir: dir, serve: map[string][]byte{}}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	lister, dial := pool(t, ws)
	h := &harness{pms: p, count: &counter{}}
	h.relay = &relay.Relay{
		Workers: lister, Dial: dial, PMS: plexsession.PMS{Addr: srv.URL, HTTP: srv.Client()}, Count: h.count,
		Refused: func(r video.Reason) { h.refs = append(h.refs, r) },
	}
	h.req = &pb.ExecRequest{TargetBinary: "Plex Transcoder", Args: libraryArgs(t), Env: map[string]string{"X_PLEX_TOKEN": "tok"}, Cwd: dir}
	return h
}

func seg(n int, startS, endS float64, next bool) *videopb.Down {
	us := func(s float64) int64 { return int64(math.Round(s * 1e6)) }
	s := &videopb.Segment{
		Name: video.SegmentName("media-%05d.ts", n), Data: []byte("segment"), StartUs: us(startS), EndUs: us(endS),
		Streams: []*videopb.Span{{Stream: 0, StartUs: us(startS), EndUs: us(endS)}, {Stream: 1, StartUs: us(startS), EndUs: us(endS - 0.08)}},
	}
	if next {
		s.NextStartUs = proto.Int64(us(endS))
	}
	return &videopb.Down{Kind: &videopb.Down_Segment{Segment: s}}
}

func done(msg string, reason video.Reason) *videopb.Down {
	return &videopb.Down{Kind: &videopb.Down_Done{Done: &videopb.Done{Error: msg, Reason: string(reason)}}}
}

func script(downs ...*videopb.Down) func(context.Context, func(*videopb.Down) error) error {
	return func(_ context.Context, send func(*videopb.Down) error) error {
		for _, d := range downs {
			if err := send(d); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestSegmentsLandBeforeTheirLine(t *testing.T) {
	h := setup(t, map[string]*worker{"video-0": {run: script(
		seg(0, 0, 8.008, true), seg(1, 8.008, 16.016, true), seg(2, 16.016, 20.5, false), done("", ""),
	)}})
	s := &sink{}
	require.NoError(t, h.relay.Execute(t.Context(), h.req, s))
	require.EqualValues(t, 0, s.exit(), s.stderr())

	require.Len(t, h.pms.lists, 5, "finished and opened after each segment but the last")
	assert.True(t, strings.HasSuffix(h.pms.lists[1], "#media-00001.ts,8.008000,8.008000,8.008000,8.008000\n"), h.pms.lists[1])
	assert.Equal(t, "media-00000.ts,0.000000,8.008000,7.928000,8.008000\nmedia-00001.ts,8.008000,16.016000,15.936000,16.016000\nmedia-00002.ts,16.016000,20.500000,20.420000,20.500000\n", h.pms.lists[4])
	for i, body := range h.pms.lists {
		for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
			if !strings.HasPrefix(line, "#") {
				assert.Contains(t, h.pms.present[i], strings.Split(line, ",")[0], "list %d names a file not yet in place", i)
			}
		}
	}
	for _, tok := range h.pms.tokens {
		assert.Equal(t, "tok", tok)
	}
	assert.Equal(t, 1, h.count.begun)
	assert.Equal(t, 1, h.count.done)
}

func TestAFileNameOutsidePlexsPatternFailsTheJob(t *testing.T) {
	bad := seg(0, 0, 8, true)
	bad.GetSegment().Name = "../x.ts"
	h := setup(t, map[string]*worker{"video-0": {run: script(bad, done("", ""))}})
	s := &sink{}
	require.Error(t, h.relay.Execute(t.Context(), h.req, s))
	assert.EqualValues(t, 1, s.exit())
	assert.Contains(t, s.stderr(), `video: worker-error: refusing file name "../x.ts"`)
	_, err := os.Stat(filepath.Join(filepath.Dir(h.req.Cwd), "x.ts"))
	assert.True(t, os.IsNotExist(err))
	assert.Empty(t, h.pms.lists)
}

func TestASegmentOutOfOrderFailsTheJob(t *testing.T) {
	h := setup(t, map[string]*worker{"video-0": {run: script(seg(1, 8, 16, true), done("", ""))}})
	s := &sink{}
	require.Error(t, h.relay.Execute(t.Context(), h.req, s))
	assert.Contains(t, s.stderr(), "video: worker-error: segment list: segment \"media-00001.ts\" out of order")
}

func TestOnlyPlexsProgressReachesPlex(t *testing.T) {
	progress := func(path, query string) *videopb.Down {
		return &videopb.Down{Kind: &videopb.Down_Progress{Progress: &videopb.Progress{Path: path, Query: query}}}
	}
	h := setup(t, map[string]*worker{"video-0": {run: script(
		progress("", "progress=1.5&size=-1&remaining=60&speed=3.1&vdec_hw_status=1"),
		progress("", "evil=1"), progress("../../library", "progress=1"), done("", ""),
	)}})
	s := &sink{}
	require.NoError(t, h.relay.Execute(t.Context(), h.req, s))
	require.Len(t, h.pms.calls, 1)
	assert.True(t, strings.HasSuffix(h.pms.calls[0], "/progress?progress=1.5&size=-1&remaining=60&speed=3.1&vdec_hw_status=1"), h.pms.calls[0])
	assert.Equal(t, []string{"tok"}, h.pms.tokens)
}

func TestAFullPoolIsRefusedFull(t *testing.T) {
	h := setup(t, map[string]*worker{"video-0": {refuse: codes.ResourceExhausted}, "video-1": {refuse: codes.ResourceExhausted}})
	s := &sink{}
	require.Error(t, h.relay.Execute(t.Context(), h.req, s))
	assert.EqualValues(t, 1, s.exit())
	assert.Contains(t, s.stderr(), "video: full: ")
	assert.Equal(t, []video.Reason{video.ReasonFull}, h.refs)
	assert.Zero(t, h.count.begun, "a refused job is no conversion")
}

func TestAFullWorkerIsPassedOver(t *testing.T) {
	served := &worker{run: script(seg(0, 0, 8, false), done("", ""))}
	h := setup(t, map[string]*worker{"video-0": {refuse: codes.ResourceExhausted}, "video-1": served, "video-2": {refuse: codes.ResourceExhausted}})
	s := &sink{}
	require.NoError(t, h.relay.Execute(t.Context(), h.req, s))
	assert.EqualValues(t, 0, s.exit(), s.stderr())
	assert.NotEmpty(t, served.job)
}

func TestNoReadyWorkerIsNoPool(t *testing.T) {
	h := setup(t, map[string]*worker{})
	s := &sink{}
	require.Error(t, h.relay.Execute(t.Context(), h.req, s))
	assert.Contains(t, s.stderr(), "video: no-pool: ")
	assert.Equal(t, []video.Reason{video.ReasonNoPool}, h.refs)
}

func TestAShapeTheWorkerCannotRunIsRefusedShape(t *testing.T) {
	h := setup(t, map[string]*worker{"video-0": {refuse: codes.InvalidArgument}})
	s := &sink{}
	require.Error(t, h.relay.Execute(t.Context(), h.req, s))
	assert.Contains(t, s.stderr(), "video: shape: ")
}

func TestAWorkerThatDiesMidStreamExitsNonZero(t *testing.T) {
	h := setup(t, map[string]*worker{"video-0": {run: func(ctx context.Context, send func(*videopb.Down) error) error {
		if err := send(seg(0, 0, 8.008, true)); err != nil {
			return err
		}
		return status.Error(codes.Unavailable, "the pod went away")
	}}})
	s := &sink{}
	require.Error(t, h.relay.Execute(t.Context(), h.req, s))
	assert.EqualValues(t, 1, s.exit())
	assert.Contains(t, s.stderr(), "video: worker-error: ")
	assert.Equal(t, 1, h.count.done, "the conversion is counted out")
	for _, body := range h.pms.lists {
		for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
			if !strings.HasPrefix(line, "#") {
				_, err := os.Stat(filepath.Join(h.req.Cwd, strings.Split(line, ",")[0]))
				assert.NoError(t, err, "a list names a missing file")
			}
		}
	}
}

func TestAStreamedInputIsReadFromPlexWithTheToken(t *testing.T) {
	live := bytes.Repeat([]byte("TS packet bytes."), 64<<10) // 1 MiB
	w := &worker{eof: make(chan struct{})}
	w.run = func(ctx context.Context, send func(*videopb.Down) error) error {
		select {
		case <-w.eof:
		case <-ctx.Done():
			return ctx.Err()
		}
		return send(done("", ""))
	}
	h := setup(t, map[string]*worker{"video-0": w})
	h.pms.serve["/livetv/sessions/abc/index.ts"] = live
	for i, a := range h.req.Args {
		if a == "-i" {
			h.req.Args[i+1] = "http://127.0.0.1:32400/livetv/sessions/abc/index.ts"
		}
	}
	s := &sink{}
	require.NoError(t, h.relay.Execute(t.Context(), h.req, s), s.stderr())
	w.mu.Lock()
	defer w.mu.Unlock()
	assert.True(t, bytes.Equal(live, w.input.Bytes()), "the worker got %d of %d bytes", w.input.Len(), len(live))
	assert.False(t, bytes.Contains(w.job, []byte("livetv/sessions")), "the worker was told the input's URL")
}

func TestACancelledJobIsCancelled(t *testing.T) {
	started := make(chan struct{})
	h := setup(t, map[string]*worker{"video-0": {run: func(ctx context.Context, _ func(*videopb.Down) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}})
	ctx, cancel := context.WithCancel(t.Context())
	go func() { <-started; cancel() }()
	s := &sink{}
	require.Error(t, h.relay.Execute(ctx, h.req, s))
	assert.Contains(t, s.stderr(), "video: cancelled: ")
	assert.Equal(t, []video.Reason{video.ReasonCancelled}, h.refs)
}

func TestTheStartupOverheadIsObserved(t *testing.T) {
	h := setup(t, map[string]*worker{"video-0": {run: script(seg(0, 0, 8, false), done("", ""))}})
	clock := time.Unix(1_790_000_000, 0)
	var got []time.Duration
	h.relay.Now = func() time.Time { t := clock; clock = clock.Add(10 * time.Second); return t }
	h.relay.Startup = func(d time.Duration) { got = append(got, d) }
	require.NoError(t, h.relay.Execute(t.Context(), h.req, &sink{}))
	assert.Equal(t, []time.Duration{2 * time.Second}, got, "10 s to the first segment, which covers 8 s of input")
}

func TestTheWorkerNeverSeesTheToken(t *testing.T) {
	w := &worker{run: script(done("", ""))}
	h := setup(t, map[string]*worker{"video-0": w})
	h.req.Env["X_PLEX_TOKEN"] = "a-real-looking-token-1234"
	require.NoError(t, h.relay.Execute(t.Context(), h.req, &sink{}))
	assert.False(t, bytes.Contains(w.job, []byte("a-real-looking-token-1234")))
	assert.False(t, bytes.Contains(w.job, []byte("127.0.0.1:32400")), "the worker was told a Plex URL")
}

func TestPlexsThrottleAnswerHoldsTheWorker(t *testing.T) {
	// Finding P: PMS answers a progress PUT canThrottle="1" while the job is
	// its throttle buffer ahead of the client. Plex's transcoder then logs
	// sloth mode and makes nothing, PUTting progress without its speed every
	// half second, until a plain answer, when it logs getting back to work.
	progress := &videopb.Down{Kind: &videopb.Down_Progress{Progress: &videopb.Progress{
		Query: "progress=4.6&size=-1&remaining=3767&speed=2.4&vdec_hw_status=0",
	}}}
	h := setup(t, map[string]*worker{"video-0": {run: script(
		progress, seg(0, 0, 8.008, true), seg(1, 8.008, 16.016, false), done("", ""),
	)}})
	h.relay.ThrottlePoll = 20 * time.Millisecond
	h.pms.throttled.Store(true)
	s := &sink{}
	ran := make(chan error, 1)
	go func() { ran <- h.relay.Execute(t.Context(), h.req, s) }()

	puts := func(calls []string) (n int) {
		for _, c := range calls {
			if strings.HasPrefix(c, "PUT ") && strings.Contains(c, "/progress?") {
				n++
			}
		}
		return n
	}
	require.Eventually(t, func() bool { calls, _ := h.pms.snapshot(); return puts(calls) >= 4 }, 5*time.Second, 5*time.Millisecond)
	calls, lists := h.pms.snapshot()
	assert.Empty(t, lists, "no segment is listed while Plex throttles")
	assert.NoFileExists(t, filepath.Join(h.req.Cwd, "media-00000.ts"))
	assert.True(t, slices.ContainsFunc(calls, func(c string) bool {
		return strings.HasPrefix(c, "POST ") && strings.HasSuffix(c, "/progress/log?level=3&message=Throttle%20-%20Going%20into%20sloth%20mode.")
	}), "%v", calls)
	assert.True(t, slices.ContainsFunc(calls, func(c string) bool {
		return strings.HasSuffix(c, "/progress?progress=4.6&size=-1&remaining=2147483647&vdec_hw_status=0")
	}), "sloth mode's PUT carries no speed and remaining=2147483647: %v", calls)

	h.pms.throttled.Store(false)
	select {
	case err := <-ran:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the relay stayed held after a plain answer")
	}
	calls, lists = h.pms.snapshot()
	assert.Len(t, lists, 3, "both segments listed once released")
	assert.True(t, slices.ContainsFunc(calls, func(c string) bool {
		return strings.HasSuffix(c, "/progress/log?level=3&message=Throttle%20-%20Getting%20back%20to%20work.")
	}), "%v", calls)
	assert.EqualValues(t, 0, s.exit(), s.stderr())
}

func TestAFailedThrottleAnswerReleasesTheWorker(t *testing.T) {
	// Plex's transcoder carries on past a failed progress PUT; a relay held
	// on a PMS that stopped answering would wedge the job instead.
	progress := &videopb.Down{Kind: &videopb.Down_Progress{Progress: &videopb.Progress{Query: "progress=1&size=-1&remaining=60&speed=2.0&vdec_hw_status=0"}}}
	h := setup(t, map[string]*worker{"video-0": {run: script(progress, seg(0, 0, 8.008, false), done("", ""))}})
	h.relay.ThrottlePoll = 20 * time.Millisecond
	h.pms.throttled.Store(true)
	s := &sink{}
	ran := make(chan error, 1)
	go func() { ran <- h.relay.Execute(t.Context(), h.req, s) }()
	require.Eventually(t, func() bool {
		calls, _ := h.pms.snapshot()
		return slices.ContainsFunc(calls, func(c string) bool { return strings.Contains(c, "sloth") })
	}, 5*time.Second, 5*time.Millisecond)
	h.pms.failing.Store(true)
	select {
	case err := <-ran:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the relay stayed held on failing answers")
	}
	_, lists := h.pms.snapshot()
	assert.Len(t, lists, 1)
}
