package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
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

func TestSupervisorStartsPlexEvenWhenRedirectFails(t *testing.T) {
	var logs bytes.Buffer
	called := false
	sup := &Supervisor{
		Binary:   fakePMS(t, gracefulPMS),
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
		Redirect: func(context.Context) error { called = true; return errors.New("iptables: not permitted") },
	}
	require.NoError(t, sup.Start(context.Background()))
	defer func() { _ = sup.Stop(context.Background()) }()

	assert.True(t, called)
	assert.Eventually(t, func() bool { return strings.Contains(logs.String(), "pms-ready") }, 3*time.Second, 20*time.Millisecond)
	assert.Contains(t, logs.String(), "not permitted")
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
