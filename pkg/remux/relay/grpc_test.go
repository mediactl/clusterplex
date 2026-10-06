package relay

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/mediactl/clusterplex/proto/remuxpb"
)

// bigWorker sends one segment larger than grpc-go's default 4 MiB receive
// limit, as 10 s of high-bitrate 1080p or any 4K remux does.
type bigWorker struct {
	remuxpb.UnimplementedRemuxServer
	size int
}

func (w bigWorker) Remux(_ *remuxpb.Job, out remuxpb.Remux_RemuxServer) error {
	if err := out.Send(&remuxpb.Event{Kind: &remuxpb.Event_File{File: &remuxpb.File{Name: "chunk-stream0-00001.m4s", Data: make([]byte, w.size)}}}); err != nil {
		return err
	}
	return out.Send(done(""))
}

// Review finding C2.
func TestASegmentOverFourMiBArrivesOverRealGRPC(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	remuxpb.RegisterRemuxServer(srv, bigWorker{size: 6 << 20})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	r, _, req := setup(t, nil, nil)
	r.Workers = workers{{Name: "remux-0", Addr: lis.Addr().String()}}
	r.Dial = GRPCDialer
	handled, err := r.Execute(t.Context(), req, &sink{})
	require.True(t, handled)
	require.NoError(t, err)
	fi, err := os.Stat(filepath.Join(req.Cwd, "chunk-stream0-00001.m4s"))
	require.NoError(t, err)
	assert.Equal(t, int64(6<<20), fi.Size())
}
