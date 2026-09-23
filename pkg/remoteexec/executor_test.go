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

func TestExecutorAddsItsOwnEnvironmentToEveryJob(t *testing.T) {
	// Plex strips LD_PRELOAD from its own environment once the interposer has
	// loaded, so a helper launched through the shim arrives here without it
	// and would open an empty local SQLite instead of the shared library. The
	// analysis then finds nothing, media_streams stays empty, and playback
	// fails with "video has neither a video stream nor an audio stream".
	bin := t.TempDir()
	writeScript(t, bin, "Plex Media Scanner.real", `echo "preload:$LD_PRELOAD"`)

	ex := &Executor{BinDir: bin, Env: []string{"LD_PRELOAD=/lib/shim.so"}}
	sink := &collectSink{}
	err := ex.Run(context.Background(), &pb.ExecRequest{
		TargetBinary: "Plex Media Scanner",
		Env:          map[string]string{"PATH": os.Getenv("PATH")},
	}, sink)
	require.NoError(t, err)

	assert.Contains(t, sink.stdout(), "preload:/lib/shim.so\n")
}

func TestExecutorEnvironmentBeatsTheRequestRatherThanDuplicatingIt(t *testing.T) {
	// Which of two values for one name a process sees is down to whichever its
	// libc finds first, so the same name must never be emitted twice. The
	// manager's value is the authoritative one: it comes from the same config
	// Plex itself is started with.
	bin := t.TempDir()
	writeScript(t, bin, "Plex Media Scanner.real", `echo "preload:$LD_PRELOAD"`)

	ex := &Executor{BinDir: bin, Env: []string{"LD_PRELOAD=/lib/right.so"}}
	sink := &collectSink{}
	err := ex.Run(context.Background(), &pb.ExecRequest{
		TargetBinary: "Plex Media Scanner",
		Env:          map[string]string{"PATH": os.Getenv("PATH"), "LD_PRELOAD": "/lib/stale.so"},
	}, sink)
	require.NoError(t, err)

	assert.Contains(t, sink.stdout(), "preload:/lib/right.so\n")
	assert.NotContains(t, sink.stdout(), "stale")
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
