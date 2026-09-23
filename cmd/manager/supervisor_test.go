package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/proxy"
)

func fakePMS(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pms.sh")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	return p
}

const gracefulPMS = "trap 'echo bye; exit 0' TERM\necho pms-ready\nwhile true; do sleep 0.1; done"

// echoUpstream stands in for Plex behind the proxy.
func echoUpstream(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return lis.Addr().String()
}

// holdConnection opens a client connection through the supervisor's proxy
// and keeps bytes moving on it until it is closed: a stream, as the drain
// understands one, rather than an idle socket.
func holdConnection(t *testing.T, sup *Supervisor) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", sup.Proxy.Addr().String())
	require.NoError(t, err)
	_, err = conn.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err = io.ReadFull(conn, make([]byte, 1))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Time{}))
	go func() {
		buf := make([]byte, 1)
		for {
			if _, err := conn.Write([]byte("x")); err != nil {
				return
			}
			if _, err := io.ReadFull(conn, buf); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	assert.Eventually(t, func() bool { return sup.Proxy.Streams() == 1 }, 2*time.Second, 10*time.Millisecond)
	return conn
}

func TestSupervisorStopLetsHeldConnectionsFinishFirst(t *testing.T) {
	// A rollout used to cut every stream on the pod the instant it began. The
	// pod has already left its Service by then, so nothing new arrives; what
	// it holds is what its clients cannot get back from another pod.
	var logs bytes.Buffer
	sup := &Supervisor{
		Binary: fakePMS(t, gracefulPMS),
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		Grace:  5 * time.Second,
		Drain:  5 * time.Second,
		Proxy:  &proxy.TCP{Listen: "127.0.0.1:0", Target: echoUpstream(t)},
	}
	require.NoError(t, sup.Start(context.Background()))
	assert.Eventually(t, func() bool { return strings.Contains(logs.String(), "pms-ready") }, 3*time.Second, 20*time.Millisecond)
	conn := holdConnection(t, sup)

	stopped := make(chan error, 1)
	go func() { stopped <- sup.Stop(context.Background()) }()
	assert.Never(t, func() bool { return strings.Contains(logs.String(), "bye") }, 500*time.Millisecond, 20*time.Millisecond,
		"Plex must not be told to stop while a client connection is held")
	assert.Contains(t, logs.String(), "draining client connections")

	require.NoError(t, conn.Close())
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return once the last connection closed")
	}
	assert.Contains(t, logs.String(), "client connections drained")
	assert.Contains(t, logs.String(), "bye", "and then Plex was stopped in the ordinary way")
}

func TestProxiedConnectionsOutliveTheStartContext(t *testing.T) {
	// The manager runs under the context its shutdown signal cancels. When
	// the proxy shared it, SIGTERM closed every client connection before the
	// drain could look: the gauge read 1 a second before the signal and the
	// drain found 0 sixteen milliseconds after it, and the client received
	// only what the kernel had already queued.
	var logs bytes.Buffer
	sup := &Supervisor{
		Binary: fakePMS(t, gracefulPMS),
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		Grace:  5 * time.Second,
		Drain:  5 * time.Second,
		Proxy:  &proxy.TCP{Listen: "127.0.0.1:0", Target: echoUpstream(t)},
	}
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, sup.Start(ctx))
	assert.Eventually(t, func() bool { return strings.Contains(logs.String(), "pms-ready") }, 3*time.Second, 20*time.Millisecond)
	conn := holdConnection(t, sup)

	cancel() // the signal
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 1, sup.Proxy.Open(), "a held connection must survive the signal; only Stop may close it")
	assert.Equal(t, 1, sup.Proxy.Streams(), "and it is still moving")

	stopped := make(chan error, 1)
	go func() { stopped <- sup.Stop(context.Background()) }()
	assert.Eventually(t, func() bool { return strings.Contains(logs.String(), "draining client connections") }, 2*time.Second, 20*time.Millisecond,
		"and Stop finds it to drain")
	require.NoError(t, conn.Close())
	require.NoError(t, <-stopped)
	assert.Contains(t, logs.String(), "client connections drained")
}

func TestSupervisorStopGivesUpDrainingAtTheDeadline(t *testing.T) {
	var logs bytes.Buffer
	sup := &Supervisor{
		Binary: fakePMS(t, gracefulPMS),
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		Grace:  5 * time.Second,
		Drain:  300 * time.Millisecond,
		Proxy:  &proxy.TCP{Listen: "127.0.0.1:0", Target: echoUpstream(t)},
	}
	require.NoError(t, sup.Start(context.Background()))
	assert.Eventually(t, func() bool { return strings.Contains(logs.String(), "pms-ready") }, 3*time.Second, 20*time.Millisecond)
	conn := holdConnection(t, sup)
	defer func() { _ = conn.Close() }()

	started := time.Now()
	require.NoError(t, sup.Stop(context.Background()))
	assert.Less(t, time.Since(started), 3*time.Second)
	assert.Contains(t, logs.String(), "client connections still open")
	assert.Contains(t, logs.String(), "bye")
}

func TestSupervisorRestartDoesNotDrain(t *testing.T) {
	// A Plex that has stopped answering is holding nothing its clients could
	// still get; waiting on its connections would only delay the restart.
	var logs bytes.Buffer
	sup := &Supervisor{
		Binary: fakePMS(t, gracefulPMS),
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		Grace:  5 * time.Second,
		Drain:  10 * time.Second,
		Proxy:  &proxy.TCP{Listen: "127.0.0.1:0", Target: echoUpstream(t)},
	}
	require.NoError(t, sup.Start(context.Background()))
	assert.Eventually(t, func() bool { return strings.Contains(logs.String(), "pms-ready") }, 3*time.Second, 20*time.Millisecond)
	conn := holdConnection(t, sup)
	defer func() { _ = conn.Close() }()

	started := time.Now()
	require.NoError(t, sup.Restart(context.Background()))
	assert.Less(t, time.Since(started), 5*time.Second)
	assert.NotContains(t, logs.String(), "draining client connections")
	require.NoError(t, sup.Stop(context.Background()))
}

func TestSupervisorStopTerminatesPlexGracefully(t *testing.T) {
	var logs bytes.Buffer
	var unexpected atomic.Int32
	sup := &Supervisor{
		Binary:           fakePMS(t, gracefulPMS),
		Logger:           slog.New(slog.NewTextHandler(&logs, nil)),
		Grace:            5 * time.Second,
		OnUnexpectedExit: func(error) { unexpected.Add(1) },
	}
	require.NoError(t, sup.Start(context.Background()))
	assert.Eventually(t, func() bool { return strings.Contains(logs.String(), "pms-ready") }, 3*time.Second, 20*time.Millisecond)

	require.NoError(t, sup.Stop(context.Background()))

	assert.Contains(t, logs.String(), "bye", "Plex must receive SIGTERM and get to flush")
	assert.Equal(t, int32(0), unexpected.Load())
}

func TestSupervisorReportsUnexpectedExit(t *testing.T) {
	exited := make(chan error, 1)
	sup := &Supervisor{
		Binary:           fakePMS(t, "exit 2"),
		Logger:           slog.Default(),
		OnUnexpectedExit: func(err error) { exited <- err },
	}
	require.NoError(t, sup.Start(context.Background()))

	select {
	case err := <-exited:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exit status 2")
	case <-time.After(3 * time.Second):
		t.Fatal("unexpected exit was not reported")
	}
}

func TestSupervisorStartsPlexThroughStartProcessWhenOneIsSet(t *testing.T) {
	var logs bytes.Buffer
	var got *exec.Cmd
	sup := &Supervisor{
		Binary:       fakePMS(t, gracefulPMS),
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
		StartProcess: func(c *exec.Cmd) error { got = c; return c.Start() },
	}
	require.NoError(t, sup.Start(context.Background()))
	defer func() { _ = sup.Stop(context.Background()) }()

	require.NotNil(t, got, "Plex must be started through the seam, not directly")
	assert.Equal(t, sup.Binary, got.Path)
	assert.Eventually(t, func() bool { return strings.Contains(logs.String(), "pms-ready") }, 3*time.Second, 20*time.Millisecond)
}

func TestPlexRunsUnderTheSubreaperWhenOneIsConfigured(t *testing.T) {
	// Plex re-execs itself through vfork. The original process exits cleanly
	// and the replacement is reparented away, so a supervisor watching only
	// the process it started sees a healthy exit and restarts the pod, on a
	// loop. The subreaper claims the descendants and stays alive until the
	// last one is gone.
	var got *exec.Cmd
	plex := fakePMS(t, gracefulPMS)
	sup := &Supervisor{
		Binary:       plex,
		Subreaper:    "/bin/sh",
		Logger:       slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		StartProcess: func(c *exec.Cmd) error { got = c; return c.Start() },
	}
	require.NoError(t, sup.Start(context.Background()))
	defer func() { _ = sup.Stop(context.Background()) }()

	require.NotNil(t, got)
	assert.Equal(t, "/bin/sh", got.Path, "the subreaper is what we launch")
	assert.Equal(t, []string{"/bin/sh", plex}, got.Args, "and Plex is what it launches")
}

func TestTheStateDirectoriesPlexExpectsAreCreatedBeforeItStarts(t *testing.T) {
	// Plex stats these rather than creating them, and an absent one is an
	// uncaught boost::filesystem exception rather than a handled error. On a
	// fresh volume none of them exist.
	dir := t.TempDir()
	sup := &Supervisor{
		Binary:       fakePMS(t, gracefulPMS),
		StateDir:     dir,
		Logger:       slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		StartProcess: func(c *exec.Cmd) error { return c.Start() },
	}
	require.NoError(t, sup.Start(context.Background()))
	defer func() { _ = sup.Stop(context.Background()) }()

	for _, name := range plexStateDirs {
		assert.DirExists(t, filepath.Join(dir, name))
	}
}

func TestExistingStateDirectoriesAreLeftAlone(t *testing.T) {
	// The directory is a persistent volume carrying a real library, so this
	// runs on every start against data that must not be disturbed.
	dir := t.TempDir()
	metadata := filepath.Join(dir, "Metadata")
	require.NoError(t, os.MkdirAll(metadata, 0o755))
	keep := filepath.Join(metadata, "keep")
	require.NoError(t, os.WriteFile(keep, []byte("library"), 0o644))

	sup := &Supervisor{
		Binary:       fakePMS(t, gracefulPMS),
		StateDir:     dir,
		Logger:       slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		StartProcess: func(c *exec.Cmd) error { return c.Start() },
	}
	require.NoError(t, sup.Start(context.Background()))
	defer func() { _ = sup.Stop(context.Background()) }()

	body, err := os.ReadFile(keep)
	require.NoError(t, err)
	assert.Equal(t, "library", string(body))
}

func TestPlexRunsDirectlyWhenNoSubreaperIsConfigured(t *testing.T) {
	var got *exec.Cmd
	sup := &Supervisor{
		Binary:       fakePMS(t, gracefulPMS),
		Logger:       slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		StartProcess: func(c *exec.Cmd) error { got = c; return c.Start() },
	}
	require.NoError(t, sup.Start(context.Background()))
	defer func() { _ = sup.Stop(context.Background()) }()

	require.NotNil(t, got)
	assert.Equal(t, sup.Binary, got.Path)
}

// Unlike the old port redirect there is no fallback. Plex binds 32400 itself,
// and in the pod namespace that is the proxy's port: starting it outside its
// own namespace means an immediate "Address in use" with the reason only in
// Plex's log.
func TestSupervisorRefusesToStartPlexOutsideItsNetworkNamespace(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	sup := &Supervisor{
		Binary:       fakePMS(t, "touch "+marker+"\n"+gracefulPMS),
		Logger:       slog.Default(),
		StartProcess: func(*exec.Cmd) error { return errors.New("operation not permitted") },
	}

	err := sup.Start(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not permitted")
	assert.NoFileExists(t, marker, "Plex must not start in the pod namespace")
	assert.NoError(t, sup.Stop(context.Background()), "a failed start leaves nothing to stop")
}

func TestSupervisorStopIsSafeBeforeStart(t *testing.T) {
	sup := &Supervisor{Binary: "/nonexistent", Logger: slog.Default()}
	assert.NoError(t, sup.Stop(context.Background()))
}

func TestSupervisorRemovesStalePIDFileBeforeStart(t *testing.T) {
	pid := filepath.Join(t.TempDir(), "plexmediaserver.pid")
	require.NoError(t, os.WriteFile(pid, []byte("32\n"), 0o644))
	sup := &Supervisor{Binary: fakePMS(t, gracefulPMS), Logger: slog.Default(), PIDFile: pid}

	require.NoError(t, sup.Start(context.Background()))
	defer func() { _ = sup.Stop(context.Background()) }()

	_, err := os.Stat(pid)
	assert.True(t, os.IsNotExist(err), "a stale pid file must be removed: Plex refuses to start when it names a live pid")
}

func TestSupervisorAppliesPreferencesBeforeStartingPlex(t *testing.T) {
	// Plex reads Preferences.xml once at startup, so a hook that ran after the
	// process started would have no effect until the next restart.
	order := filepath.Join(t.TempDir(), "order")
	sup := &Supervisor{
		Binary: fakePMS(t, "echo pms >> "+order+"\n"+gracefulPMS),
		Logger: slog.Default(),
		Preferences: func(context.Context) error {
			return os.WriteFile(order, []byte("prefs\n"), 0o600)
		},
	}
	require.NoError(t, sup.Start(context.Background()))
	defer func() { _ = sup.Stop(context.Background()) }()

	assert.Eventually(t, func() bool {
		b, err := os.ReadFile(order)
		return err == nil && string(b) == "prefs\npms\n"
	}, 3*time.Second, 20*time.Millisecond, "preferences must be written before Plex starts")
}

func TestSupervisorRefusesToStartWhenPreferencesCannotBeApplied(t *testing.T) {
	// Unlike the port redirect there is no fallback: starting Plex anyway
	// would run it with settings that silently differ from the declared ones.
	marker := filepath.Join(t.TempDir(), "started")
	sup := &Supervisor{
		Binary:      fakePMS(t, "touch "+marker+"\n"+gracefulPMS),
		Logger:      slog.Default(),
		Preferences: func(context.Context) error { return errors.New("read-only file system") },
	}

	err := sup.Start(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "read-only file system")
	assert.NoFileExists(t, marker, "Plex must not start")
	assert.NoError(t, sup.Stop(context.Background()), "a failed start leaves nothing to stop")
}
