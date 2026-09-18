package main

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/mediactl/clusterplex/proto"
)

// MockManager is a simple grpc server to test the shim
type MockManager struct {
	pb.UnimplementedManagerServer
}

func (m *MockManager) ExecuteRemote(req *pb.ExecRequest, stream pb.Manager_ExecuteRemoteServer) error {
	stream.Send(&pb.TranscodeLog{
		StdoutChunk: []byte("hello from mock"),
		IsFinished:  true,
		ExitCode:    0,
	})
	return nil
}

func TestShimIntegration(t *testing.T) {
	socketPath := "/tmp/test-cluster-plex.sock"
	os.Remove(socketPath)

	// 1. Start mock Manager
	lis, err := net.Listen("unix", socketPath)
	assert.NoError(t, err)

	grpcServer := grpc.NewServer()
	pb.RegisterManagerServer(grpcServer, &MockManager{})
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	// 2. Dial from client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, "unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	assert.NoError(t, err)
	defer conn.Close()

	client := pb.NewManagerClient(conn)

	// 3. Send request
	req := &pb.ExecRequest{
		TargetBinary: "Plex Transcoder",
		Args:         []string{"-i", "test.mkv"},
	}

	stream, err := client.ExecuteRemote(ctx, req)
	assert.NoError(t, err)

	var output []byte
	for {
		logData, err := stream.Recv()
		if err == io.EOF {
			break
		}
		assert.NoError(t, err)
		output = append(output, logData.StdoutChunk...)
		if logData.IsFinished {
			assert.Equal(t, int32(0), logData.ExitCode)
			break
		}
	}

	assert.Equal(t, "hello from mock", string(output))
}
