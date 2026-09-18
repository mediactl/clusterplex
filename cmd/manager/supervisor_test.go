package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakePMS(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pms.sh")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	return p
}

const gracefulPMS = "trap 'echo bye; exit 0' TERM\necho pms-ready\nwhile true; do sleep 0.1; done"

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
