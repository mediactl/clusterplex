package remoteexec

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/mediactl/clusterplex/proto"
)

// collectSink gathers every message an Executor or Dispatcher streams.
type collectSink struct {
	mu   sync.Mutex
	msgs []*pb.TranscodeLog
}

func (c *collectSink) Send(m *pb.TranscodeLog) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	return nil
}

func (c *collectSink) stdout() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, m := range c.msgs {
		b.Write(m.StdoutChunk)
	}
	return b.String()
}

func (c *collectSink) stderr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, m := range c.msgs {
		b.Write(m.StderrChunk)
	}
	return b.String()
}

func (c *collectSink) final() *pb.TranscodeLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.msgs) - 1; i >= 0; i-- {
		if c.msgs[i].IsFinished {
			return c.msgs[i]
		}
	}
	return nil
}

// writeScript drops an executable /bin/sh script named name into dir.
func writeScript(t *testing.T, dir, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755))
}

// startServer serves svc on a unix socket and returns the socket path.
func startServer(t *testing.T, svc pb.ManagerServer) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "m.sock")
	lis, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := grpc.NewServer()
	pb.RegisterManagerServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return sock
}

func unixDialer(sock string) Dialer {
	return func(_ context.Context, _ string) (pb.ManagerClient, io.Closer, error) {
		conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, nil, err
		}
		return pb.NewManagerClient(conn), conn, nil
	}
}

func drain(t *testing.T, stream pb.Manager_ExecuteRemoteClient) (string, *pb.TranscodeLog) {
	t.Helper()
	var out strings.Builder
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out.String(), nil
		}
		require.NoError(t, err)
		out.Write(m.StdoutChunk)
		if m.IsFinished {
			return out.String(), m
		}
	}
}
