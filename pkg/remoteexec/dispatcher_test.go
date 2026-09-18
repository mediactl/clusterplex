package remoteexec

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/mediactl/clusterplex/proto"
)

type staticWorkers []Worker

func (s staticWorkers) ListReady(context.Context) ([]Worker, error) { return s, nil }

// recordingServer answers every request with one canned message and keeps the request.
type recordingServer struct {
	pb.UnimplementedManagerServer
	mu   sync.Mutex
	reqs []*pb.ExecRequest
}

func (r *recordingServer) ExecuteRemote(req *pb.ExecRequest, stream pb.Manager_ExecuteRemoteServer) error {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	return stream.Send(&pb.TranscodeLog{StdoutChunk: []byte("remote"), IsFinished: true})
}

type failingServer struct{ pb.UnimplementedManagerServer }

func (failingServer) ExecuteRemote(*pb.ExecRequest, pb.Manager_ExecuteRemoteServer) error {
	return status.Error(codes.Unavailable, "worker draining")
}

func noDial(t *testing.T) Dialer {
	return func(context.Context, string) (pb.ManagerClient, io.Closer, error) {
		t.Fatal("dial must not be called for a local job")
		return nil, nil, nil
	}
}

func localScanner(t *testing.T) *Executor {
	bin := t.TempDir()
	writeScript(t, bin, "Plex Media Scanner.real", `echo local`)
	writeScript(t, bin, "Plex Transcoder.real", `echo local`)
	return &Executor{BinDir: bin}
}

func TestDispatcherForwardsTranscoderToWorkerWithRewrittenArgs(t *testing.T) {
	rec := &recordingServer{}
	d := &Dispatcher{
		Local:   localScanner(t),
		Workers: staticWorkers{{Name: "plex-1", Addr: "10.0.0.2:50051"}},
		Dial:    unixDialer(startServer(t, rec)),
		PMSAddr: "plex-0.plex-workers.media.svc.cluster.local:32400",
	}
	sink := &collectSink{}
	err := d.Execute(context.Background(), &pb.ExecRequest{
		TargetBinary: "Plex Transcoder",
		Args:         []string{"-progressurl", "http://127.0.0.1:32400/progress"},
		Cwd:          "/transcode/s1",
	}, sink)
	require.NoError(t, err)
	assert.Equal(t, "remote", sink.stdout())
	require.Len(t, rec.reqs, 1)
	assert.Equal(t, []string{"-progressurl", "http://plex-0.plex-workers.media.svc.cluster.local:32400/progress"}, rec.reqs[0].Args)
	assert.Equal(t, "/transcode/s1", rec.reqs[0].Cwd)
	assert.Equal(t, "Plex Transcoder", rec.reqs[0].TargetBinary)
}

func TestDispatcherRunsScannerLocallyEvenWithWorkers(t *testing.T) {
	d := &Dispatcher{Local: localScanner(t), Workers: staticWorkers{{Name: "plex-1", Addr: "x"}}, Dial: noDial(t)}
	sink := &collectSink{}
	require.NoError(t, d.Execute(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Media Scanner"}, sink))
	assert.Equal(t, "local\n", sink.stdout())
}

func TestDispatcherKeepsEasyAudioEncoderTranscodesLocal(t *testing.T) {
	d := &Dispatcher{Local: localScanner(t), Workers: staticWorkers{{Name: "plex-1", Addr: "x"}}, Dial: noDial(t)}
	sink := &collectSink{}
	req := &pb.ExecRequest{TargetBinary: "Plex Transcoder", Env: map[string]string{"EAE_ROOT": "/transcode/eae"}}
	require.NoError(t, d.Execute(context.Background(), req, sink))
	assert.Equal(t, "local\n", sink.stdout())
}

func TestDispatcherRunsLocallyWhenNoWorkerIsReady(t *testing.T) {
	d := &Dispatcher{Local: localScanner(t), Workers: staticWorkers{}, Dial: noDial(t)}
	sink := &collectSink{}
	require.NoError(t, d.Execute(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Transcoder"}, sink))
	assert.Equal(t, "local\n", sink.stdout())
}

func TestDispatcherFallsBackToLocalWhenDialFails(t *testing.T) {
	d := &Dispatcher{
		Local:   localScanner(t),
		Workers: staticWorkers{{Name: "plex-1", Addr: "x"}},
		Dial: func(context.Context, string) (pb.ManagerClient, io.Closer, error) {
			return nil, nil, errors.New("connection refused")
		},
		PMSAddr: "pms:32400",
	}
	sink := &collectSink{}
	require.NoError(t, d.Execute(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Transcoder"}, sink))
	assert.Equal(t, "local\n", sink.stdout())
}

func TestDispatcherFallsBackToLocalWhenWorkerRejectsBeforeOutput(t *testing.T) {
	d := &Dispatcher{
		Local:   localScanner(t),
		Workers: staticWorkers{{Name: "plex-1", Addr: "x"}},
		Dial:    unixDialer(startServer(t, failingServer{})),
		PMSAddr: "pms:32400",
	}
	sink := &collectSink{}
	require.NoError(t, d.Execute(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Transcoder"}, sink))
	assert.Equal(t, "local\n", sink.stdout())
}

func TestPickWorkerRoundRobin(t *testing.T) {
	d := &Dispatcher{}
	ws := []Worker{{Name: "a"}, {Name: "b"}}
	assert.Equal(t, "a", d.pick(ws).Name)
	assert.Equal(t, "b", d.pick(ws).Name)
	assert.Equal(t, "a", d.pick(ws).Name)
}
