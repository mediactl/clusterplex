package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	pb "github.com/mediactl/clusterplex/proto"
)

// recordingManager keeps the last request and answers with fixed output.
type recordingManager struct {
	pb.UnimplementedManagerServer
	mu   sync.Mutex
	last *pb.ExecRequest
}

func (m *recordingManager) ExecuteRemote(req *pb.ExecRequest, stream pb.Manager_ExecuteRemoteServer) error {
	m.mu.Lock()
	m.last = req
	m.mu.Unlock()
	if err := stream.Send(&pb.TranscodeLog{StdoutChunk: []byte("out "), StderrChunk: []byte("err ")}); err != nil {
		return err
	}
	return stream.Send(&pb.TranscodeLog{IsFinished: true, ExitCode: 7})
}

func startManager(t *testing.T, m pb.ManagerServer) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "clusterplex.sock")
	lis, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := grpc.NewServer()
	pb.RegisterManagerServer(srv, m)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return sock
}

func TestRunForwardsInvocationAndReturnsExitCode(t *testing.T) {
	m := &recordingManager{}
	sock := startManager(t, m)
	var stdout, stderr bytes.Buffer

	code := run(context.Background(), sock, "/usr/lib/plexmediaserver/Plex Transcoder", []string{"-i", "x.mkv"}, &stdout, &stderr)

	assert.Equal(t, 7, code)
	assert.Equal(t, "out ", stdout.String())
	assert.Equal(t, "err ", stderr.String())
	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NotNil(t, m.last)
	assert.Equal(t, "Plex Transcoder", m.last.TargetBinary)
	assert.Equal(t, []string{"-i", "x.mkv"}, m.last.Args)
	assert.Equal(t, wd, m.last.Cwd)
	assert.Equal(t, os.Getenv("PATH"), m.last.Env["PATH"])
}

func TestRunFailsLoudlyWhenManagerIsUnreachable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sock := filepath.Join(t.TempDir(), "missing.sock")

	code := run(context.Background(), sock, "Plex Transcoder", nil, &stdout, &stderr)

	assert.NotEqual(t, 0, code)
	assert.Contains(t, stderr.String(), "missing.sock")
}
