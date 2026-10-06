package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/mediactl/clusterplex/pkg/remoteexec"
	pb "github.com/mediactl/clusterplex/proto"
	"github.com/mediactl/clusterplex/proto/remuxpb"
)

// pms records what reaches Plex and, for each manifest, which files the
// session directory held at that moment.
type pms struct {
	mu        sync.Mutex
	dir       string
	calls     []string
	tokens    []string
	atPublish [][]string
}

func (p *pms) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	p.tokens = append(p.tokens, r.Header.Get("X-Plex-Token"))
	if r.Method == http.MethodPost {
		ents, _ := os.ReadDir(p.dir)
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		p.atPublish = append(p.atPublish, names)
	}
}

type players struct{}

func (players) PlayerProduct(context.Context, string) (string, error) { return "Plex Web", nil }

type workers []remoteexec.Worker

func (w workers) ListReady(context.Context) ([]remoteexec.Worker, error) { return w, nil }

type events struct {
	grpc.ClientStream
	evs []*remuxpb.Event
	err error // returned after evs
}

func (e *events) Recv() (*remuxpb.Event, error) {
	if len(e.evs) == 0 {
		if e.err != nil {
			return nil, e.err
		}
		return nil, io.EOF
	}
	ev := e.evs[0]
	e.evs = e.evs[1:]
	return ev, nil
}
func (e *events) Header() (metadata.MD, error) { return nil, nil }

type client struct{ s *events }

func (c client) Remux(context.Context, *remuxpb.Job, ...grpc.CallOption) (remuxpb.Remux_RemuxClient, error) {
	return c.s, nil
}

type sink struct{ logs []*pb.TranscodeLog }

func (s *sink) Send(m *pb.TranscodeLog) error { s.logs = append(s.logs, m); return nil }

func file(name string) *remuxpb.Event {
	return &remuxpb.Event{Kind: &remuxpb.Event_File{File: &remuxpb.File{Name: name, Data: []byte(name)}}}
}

func manifest() *remuxpb.Event {
	return &remuxpb.Event{Kind: &remuxpb.Event_Manifest{Manifest: &remuxpb.Manifest{Mpd: []byte("<MPD/>")}}}
}

func done(msg string) *remuxpb.Event {
	return &remuxpb.Event{Kind: &remuxpb.Event_Done{Done: &remuxpb.Done{Error: msg}}}
}

const argv = `-codec:0 hevc -codec:1 aac -i /m/a.mkv -progressurl http://127.0.0.1:32400/video/:/transcode/session/s1/u1/progress -map 0:0 -codec:0 copy -filter_complex "[0:1] aresample=async=1:ochl='stereo':rematrix_maxval=0.000000dB:osr=48000[0]" -map "[0]" -codec:1 aac -b:1 256k -f dash -seg_duration 5 -dash_segment_type mp4 -init_seg_name 'init-stream$RepresentationID$.m4s' -media_seg_name 'chunk-stream$RepresentationID$-$Number%05d$.m4s' -skip_to_segment 1 -manifest_name "http://127.0.0.1:32400/video/:/transcode/session/s1/u1/manifest?X-Plex-Http-Pipeline=infinite" dash`

func setup(t *testing.T, evs []*remuxpb.Event, streamErr error) (*Relay, *pms, *pb.ExecRequest) {
	t.Helper()
	dir := t.TempDir()
	p := &pms{dir: dir}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	r := &Relay{
		Players: players{}, Workers: workers{{Name: "remux-0", Addr: "10.0.0.1:50052"}},
		Dial: func(context.Context, string) (remuxpb.RemuxClient, io.Closer, error) {
			return client{&events{evs: evs, err: streamErr}}, io.NopCloser(nil), nil
		},
		PMS: srv.URL, HTTP: srv.Client(),
	}
	return r, p, &pb.ExecRequest{TargetBinary: "Plex Transcoder", Args: splitArgv(argv), Env: map[string]string{"X_PLEX_TOKEN": "tok"}, Cwd: dir}
}

func TestFilesAreWrittenBeforeTheManifestThatListsThem(t *testing.T) {
	r, p, req := setup(t, []*remuxpb.Event{
		file("init-stream0.m4s"), file("init-stream1.m4s"),
		{Kind: &remuxpb.Event_Progress{Progress: &remuxpb.Progress{Path: "", Query: "duration=30.0"}}},
		file("chunk-stream0-00001.m4s"), file("chunk-stream1-00001.m4s"), manifest(), done(""),
	}, nil)
	s := &sink{}
	handled, err := r.Execute(t.Context(), req, s)
	require.True(t, handled)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"PUT /video/:/transcode/session/s1/u1/progress?duration=30.0",
		"POST /video/:/transcode/session/s1/u1/manifest?X-Plex-Http-Pipeline=infinite",
	}, p.calls)
	assert.Equal(t, []string{"tok", "tok"}, p.tokens)
	assert.ElementsMatch(t, []string{"init-stream0.m4s", "init-stream1.m4s", "chunk-stream0-00001.m4s", "chunk-stream1-00001.m4s"}, p.atPublish[0])
	require.NotEmpty(t, s.logs)
	assert.True(t, s.logs[len(s.logs)-1].GetIsFinished())
	assert.Zero(t, s.logs[len(s.logs)-1].GetExitCode())
}

func TestANameOutsidePlexsPatternsIsRefused(t *testing.T) {
	for _, name := range []string{"../escape", "manifest.mpd", "init-stream2.m4s"} {
		r, _, req := setup(t, []*remuxpb.Event{file(name), done("")}, nil)
		s := &sink{}
		handled, err := r.Execute(t.Context(), req, s)
		assert.True(t, handled, name)
		assert.Error(t, err, name)
		for _, p := range []string{filepath.Join(req.Cwd, name), filepath.Join(filepath.Dir(req.Cwd), "escape")} {
			_, statErr := os.Stat(p)
			assert.True(t, os.IsNotExist(statErr), "%s: %s was written", name, p)
		}
	}
}

// Review focus 4.
func TestAFailedWorkerAfterOutputExitsNonZero(t *testing.T) {
	r, _, req := setup(t, []*remuxpb.Event{file("init-stream0.m4s")}, errors.New("worker gone"))
	s := &sink{}
	handled, err := r.Execute(t.Context(), req, s)
	assert.True(t, handled, "output was written: Plex must not also run the job")
	assert.Error(t, err)
	last := s.logs[len(s.logs)-1]
	assert.True(t, last.GetIsFinished())
	assert.NotZero(t, last.GetExitCode())
}

func TestAWorkerThatFailsBeforeOutputLeavesTheJobToPlex(t *testing.T) {
	r, _, req := setup(t, nil, errors.New("refused"))
	handled, err := r.Execute(t.Context(), req, &sink{})
	assert.False(t, handled)
	assert.NoError(t, err)
}

func TestNoWorkerLeavesTheJobToPlex(t *testing.T) {
	r, _, req := setup(t, nil, nil)
	r.Workers = workers{}
	handled, _ := r.Execute(t.Context(), req, &sink{})
	assert.False(t, handled)
}

func splitArgv(line string) []string {
	var out []string
	var cur strings.Builder
	in, quote := false, byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			if c == '"' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == ' ':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// Review finding I5: the worker reports a failure as Done{error}; one that
// arrives before any file (the input not mounted on the pool, a codec the
// pipeline refuses) leaves the job to Plex's transcoder.
func TestAWorkerErrorBeforeAnyFileLeavesTheJobToPlex(t *testing.T) {
	r, p, req := setup(t, []*remuxpb.Event{
		{Kind: &remuxpb.Event_Progress{Progress: &remuxpb.Progress{Query: "duration=30.0"}}},
		done("stat /library/a.mkv: no such file or directory"),
	}, nil)
	s := &sink{}
	handled, err := r.Execute(t.Context(), req, s)
	assert.False(t, handled)
	assert.NoError(t, err)
	assert.Empty(t, s.logs, "nothing reached the shim, so Plex can run the job")
	_ = p
}

// Review finding I9: the pool parses untrusted media, so a worker must not
// be able to steer the token-bearing PUTs the relay makes for it.
func TestProgressIsOnlyPlexsProgress(t *testing.T) {
	r, p, req := setup(t, []*remuxpb.Event{
		{Kind: &remuxpb.Event_Progress{Progress: &remuxpb.Progress{Path: "../../../../library/sections/1", Query: "x=1"}}},
		{Kind: &remuxpb.Event_Progress{Progress: &remuxpb.Progress{Path: "stream", Query: "index=0&codec=hevc&type=video&X-Plex-Token=evil"}}},
		{Kind: &remuxpb.Event_Progress{Progress: &remuxpb.Progress{Path: "streamDetail", Query: "index=1&codec=aac&type=audio&channels=2"}}},
		{Kind: &remuxpb.Event_Progress{Progress: &remuxpb.Progress{Path: "", Query: "progress=12.5&size=-1&remaining=40&speed=3.1"}}},
		file("init-stream0.m4s"), done(""),
	}, nil)
	_, err := r.Execute(t.Context(), req, &sink{})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"PUT /video/:/transcode/session/s1/u1/progress/streamDetail?index=1&codec=aac&type=audio&channels=2",
		"PUT /video/:/transcode/session/s1/u1/progress?progress=12.5&size=-1&remaining=40&speed=3.1",
	}, p.calls, "an unknown path or query key is dropped, never sent")
}
