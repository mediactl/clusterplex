//go:build netns

package net

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/remoteexec"
	pb "github.com/mediactl/clusterplex/proto"
)

// dialEnv names the address the helper process dials when the test binary is
// re-run as a job.
const dialEnv = "CLUSTERPLEX_TEST_DIAL"

// TestDialHelperProcess is not a test. The jobs below re-run this binary as
// their "Plex Media Scanner", and this is what it does then: dial the address
// it is given, the way a scanner calls Plex back on 127.0.0.1:32400, and print
// what it read.
func TestDialHelperProcess(t *testing.T) {
	addr := os.Getenv(dialEnv)
	if addr == "" {
		t.Skip("only runs as a job")
	}
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stdout, "dial:", err)
		os.Exit(7)
	}
	line, _ := bufio.NewReader(conn).ReadString('\n')
	_, _ = fmt.Fprint(os.Stdout, "read:", line)
	os.Exit(0)
}

type jobSink struct {
	mu  sync.Mutex
	out strings.Builder
	fin *pb.TranscodeLog
}

func (s *jobSink) Send(m *pb.TranscodeLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out.Write(m.StdoutChunk)
	if m.IsFinished {
		s.fin = m
	}
	return nil
}

// listenOnPlexLoopback stands in for Plex: a listener on 127.0.0.1 inside
// Plex's namespace, answering one line per connection.
func listenOnPlexLoopback(t *testing.T, n *Network) string {
	t.Helper()
	var lis net.Listener
	require.NoError(t, n.Do(func() error {
		var err error
		lis, err = net.Listen("tcp", "127.0.0.1:0")
		return err
	}))
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			_, _ = fmt.Fprintln(c, "plex")
			_ = c.Close()
		}
	}()
	return lis.Addr().String()
}

// runDialJob runs this binary as a "Plex Media Scanner" job through ex, dialing
// addr, and returns what it printed and its exit code.
func runDialJob(t *testing.T, ex *remoteexec.Executor, addr string) (string, int32) {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run='^TestDialHelperProcess$'\n", self)
	require.NoError(t, os.WriteFile(filepath.Join(ex.BinDir, "Plex Media Scanner.real"), []byte(script), 0o755))

	sink := &jobSink{}
	require.NoError(t, ex.Run(context.Background(), &pb.ExecRequest{
		TargetBinary: "Plex Media Scanner",
		Env:          map[string]string{dialEnv: addr, "PATH": os.Getenv("PATH")},
	}, sink))
	require.NotNil(t, sink.fin)
	return sink.out.String(), sink.fin.ExitCode
}

// The property the manager's jobs depend on: a helper started through the
// namespace reaches Plex on 127.0.0.1, as it would beside a stock Plex, and
// one started the ordinary way does not. In the pod namespace 127.0.0.1:32400
// is the proxy, and Plex sees the call arrive from its link subnet rather
// than from loopback.
func TestAJobStartedThroughTheNamespaceReachesPlexOnLoopback(t *testing.T) {
	n := provision(t)
	addr := listenOnPlexLoopback(t, n)

	inside := &remoteexec.Executor{BinDir: t.TempDir(), Start: n.StartProcess}
	out, code := runDialJob(t, inside, addr)
	assert.Equal(t, int32(0), code, out)
	assert.Contains(t, out, "read:plex\n")

	outside := &remoteexec.Executor{BinDir: t.TempDir()}
	out, code = runDialJob(t, outside, addr)
	assert.Equal(t, int32(7), code, "the pod namespace's loopback is not Plex's: %s", out)
}

// Starting through the namespace forks from a thread the runtime retires
// afterwards. The executor must still own the job's lifetime: cancelling it
// kills the whole group, and Run returns.
func TestAJobStartedThroughTheNamespaceIsStillKilledWithItsGroup(t *testing.T) {
	n := provision(t)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "Plex Transcoder.real"), []byte("#!/bin/sh\nsleep 30 & wait\n"), 0o755))
	ex := &remoteexec.Executor{BinDir: bin, Start: n.StartProcess}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &jobSink{}
	done := make(chan error, 1)
	go func() {
		done <- ex.Run(ctx, &pb.ExecRequest{TargetBinary: "Plex Transcoder", Env: map[string]string{"PATH": os.Getenv("PATH")}}, sink)
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the job's group outlived the cancellation")
	}
	require.NotNil(t, sink.fin)
	assert.NotEqual(t, int32(0), sink.fin.ExitCode)
}
