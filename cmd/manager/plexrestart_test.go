package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlexIsStartedAgainAfterItHasBeenLost(t *testing.T) {
	// Plex segfaults and the subreaper outlives it, so cmd.Wait never returns
	// and the supervisor never learns Plex is gone. Restarting Plex here is
	// seconds; replacing the pod was forty, because it re-runs the init
	// script and the election on the way back.
	var logs bytes.Buffer
	sup := &Supervisor{
		Binary: fakePMS(t, gracefulPMS),
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		Grace:  time.Second,
	}
	require.NoError(t, sup.Start(t.Context()))
	assert.Eventually(t, func() bool { return strings.Contains(logs.String(), "pms-ready") },
		3*time.Second, 20*time.Millisecond)
	first := sup.pid()

	require.NoError(t, sup.Restart(t.Context()))

	assert.NotEqual(t, first, sup.pid(), "Plex must be a new process, not the one that died")
	assert.Eventually(t, func() bool { return strings.Count(logs.String(), "pms-ready") == 2 },
		3*time.Second, 20*time.Millisecond, "the replacement has to actually come up")
	require.NoError(t, sup.Stop(context.Background()))
}

func TestRestartingPlexDoesNotReportItAsAnUnexpectedExit(t *testing.T) {
	// Stop sets the stopping flag so the wait goroutine stays quiet. A restart
	// that skipped it would call OnUnexpectedExit, and the manager would treat
	// its own recovery as another loss and escalate.
	lost := make(chan error, 4)
	sup := &Supervisor{
		Binary:           fakePMS(t, gracefulPMS),
		Logger:           slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		Grace:            time.Second,
		OnUnexpectedExit: func(err error) { lost <- err },
	}
	require.NoError(t, sup.Start(t.Context()))
	require.NoError(t, sup.Restart(t.Context()))
	t.Cleanup(func() { _ = sup.Stop(context.Background()) })

	select {
	case err := <-lost:
		t.Fatalf("a deliberate restart reported an unexpected exit: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAPlexThatKeepsDyingReplacesThePodInsteadOfRestartingForEver(t *testing.T) {
	// Restarting in place is right for a crash; it is wrong for a Plex that
	// cannot stay up, because the pod then looks ready-ish for ever while
	// serving nothing. A fresh container re-runs the init script and the
	// election, which is the cleanest recovery of last resort.
	var r plexRestarts
	for i := 1; i < plexRestartLimit; i++ {
		assert.False(t, r.record(time.Now()), "restart %d of %d stays in place", i, plexRestartLimit)
	}
	assert.True(t, r.record(time.Now()), "the %dth restart in the window gives up on the pod", plexRestartLimit)
}

func TestCrashesSpacedOutDoNotCountAsALoop(t *testing.T) {
	// A pod that crashes once a day is not looping, and replacing it on the
	// third crash a week later would be surprising.
	var r plexRestarts
	now := time.Now()
	for i := 0; i < plexRestartLimit*3; i++ {
		spaced := now.Add(time.Duration(i) * (plexRestartWindow + time.Minute))
		assert.False(t, r.record(spaced), "crash %d is outside the window", i+1)
	}
}
