package remoteexec

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/mediactl/clusterplex/proto"
)

func TestServiceStreamsExecutorOutputOverGRPC(t *testing.T) {
	bin := t.TempDir()
	writeScript(t, bin, "Plex Media Scanner.real", `echo hello; exit 0`)
	ex := &Executor{BinDir: bin}

	sock := startServer(t, &Service{Execute: ex.Run})
	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	stream, err := pb.NewManagerClient(conn).ExecuteRemote(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Media Scanner"})
	require.NoError(t, err)
	out, fin := drain(t, stream)
	assert.Equal(t, "hello\n", out)
	require.NotNil(t, fin)
	assert.Equal(t, int32(0), fin.ExitCode)
}
