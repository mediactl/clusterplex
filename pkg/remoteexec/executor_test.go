package remoteexec

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/mediactl/clusterplex/proto"
)

func TestExecutorRunsRealBinaryWithArgsEnvAndCwd(t *testing.T) {
	bin := t.TempDir()
	work := t.TempDir()
	writeScript(t, bin, "Plex Transcoder.real", `echo "out:$1"; echo "err:$2" >&2; echo "cwd:$(pwd)"; echo "env:$FOO"; exit 3`)

	ex := &Executor{BinDir: bin}
	sink := &collectSink{}
	err := ex.Run(context.Background(), &pb.ExecRequest{
		TargetBinary: "Plex Transcoder",
		Args:         []string{"a", "b"},
		Env:          map[string]string{"FOO": "bar", "PATH": os.Getenv("PATH")},
		Cwd:          work,
	}, sink)
	require.NoError(t, err)

	out := sink.stdout()
	assert.Contains(t, out, "out:a\n")
	assert.Contains(t, out, "cwd:"+work+"\n")
	assert.Contains(t, out, "env:bar\n")
	assert.Contains(t, sink.stderr(), "err:b\n")
	fin := sink.final()
	require.NotNil(t, fin, "a finished message must close the stream")
	assert.Equal(t, int32(3), fin.ExitCode)
}

func TestExecutorKillsProcessGroupWhenContextCancelled(t *testing.T) {
	bin := t.TempDir()
	writeScript(t, bin, "Plex Transcoder.real", `sleep 30`)

	ex := &Executor{BinDir: bin}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &collectSink{}
	done := make(chan error, 1)
	go func() { done <- ex.Run(ctx, &pb.ExecRequest{TargetBinary: "Plex Transcoder"}, sink) }()

	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not return after cancellation; the child sleep was not killed")
	}
	fin := sink.final()
	require.NotNil(t, fin)
	assert.NotEqual(t, int32(0), fin.ExitCode)
}

func TestExecutorRejectsPathTraversal(t *testing.T) {
	ex := &Executor{BinDir: t.TempDir()}
	err := ex.Run(context.Background(), &pb.ExecRequest{TargetBinary: "../../bin/sh"}, &collectSink{})
	require.Error(t, err)
}

func TestExecutorReportsMissingBinary(t *testing.T) {
	ex := &Executor{BinDir: t.TempDir()}
	err := ex.Run(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Transcoder"}, &collectSink{})
	require.Error(t, err)
}
