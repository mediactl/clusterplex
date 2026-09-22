package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlexIsGivenUpOnOnlyAfterItHasMissedSeveralChecksInARow(t *testing.T) {
	// One missed check is a slow request, a GC pause or a restarting proxy.
	// Restarting the pod for that would turn a hiccup into an outage.
	var h plexHealth
	boom := errors.New("connection refused")

	for i := 1; i < unhealthyRestartAfter; i++ {
		serving, giveUp := h.record(boom)
		assert.False(t, serving)
		assert.False(t, giveUp, "check %d of %d must not give up yet", i, unhealthyRestartAfter)
	}

	serving, giveUp := h.record(boom)
	assert.False(t, serving)
	assert.True(t, giveUp, "the %dth consecutive failure gives up", unhealthyRestartAfter)
}

func TestPlexIsGivenUpOnEveryRoundOfChecksRatherThanOnEveryTick(t *testing.T) {
	// Not once per tick: that would restart Plex ten times while the first
	// restart was still starting. Not once ever either, which is what this
	// did while losing Plex meant exiting — the count never reached the
	// threshold a second time, so a Plex that did not come back after a
	// restart was never tried again and the pod sat at 0/1 for ever.
	//
	// Once per round of failures is what lets the restarts escalate: each
	// round is one restart, and plexRestartLimit rounds replace the pod.
	var h plexHealth
	boom := errors.New("connection refused")
	gaveUp := 0
	for i := 0; i < unhealthyRestartAfter*4; i++ {
		if _, giveUp := h.record(boom); giveUp {
			gaveUp++
		}
	}
	assert.Equal(t, 4, gaveUp, "each run of consecutive failures is one attempt to recover")
}

func TestAnAnswerFromPlexForgivesTheFailuresBeforeIt(t *testing.T) {
	// Plex answering again means it is alive, so the count has to start over;
	// otherwise a pod that hiccups once an hour is eventually restarted for it.
	var h plexHealth
	boom := errors.New("connection refused")
	for i := 1; i < unhealthyRestartAfter; i++ {
		h.record(boom)
	}

	serving, giveUp := h.record(nil)
	assert.True(t, serving)
	assert.False(t, giveUp)

	for i := 1; i < unhealthyRestartAfter; i++ {
		_, giveUp = h.record(boom)
		assert.False(t, giveUp, "the count restarts after Plex answers")
	}
}

func TestTheHealthWatchRestartsThePodWhenPlexStopsAnswering(t *testing.T) {
	// Plex can abort its main loop while the container carries on: it runs
	// under a subreaper that stays alive as long as any descendant does, and
	// the plug-ins outlive it, so cmd.Wait never returns and OnUnexpectedExit
	// never fires. Withdrawing readiness alone left the pod 0/1 for ever with
	// nothing to bring it back.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<MediaContainer machineIdentifier="test"/>`))
	}))
	t.Cleanup(srv.Close)

	m := newTestManager()
	m.plexAddr = srv.Listener.Addr().String()
	var restarts atomic.Int32
	m.onPlexLost = func(error) { restarts.Add(1) }

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const tick = 5 * time.Millisecond
	go m.watchPlexHealth(ctx, tick, 50*time.Millisecond)

	// Nothing happens while Plex answers.
	time.Sleep(10 * tick)
	require.Zero(t, restarts.Load(), "a healthy Plex must never restart the pod")

	srv.Close()

	assert.Eventually(t, func() bool { return restarts.Load() == 1 },
		2*time.Second, tick,
		"the pod is given up on once Plex has stopped answering")
}

func TestStoppingPlexOnPurposeDoesNotCountAsLosingIt(t *testing.T) {
	// A pod that loses the lease stops Plex and carries on as a standby. That
	// is a handover, not a crash, but the watch the leader started was still
	// running: it found Plex gone — because this pod had just stopped it —
	// and restarted the container three checks later.
	//
	// In the cluster that turned every election into a restart. plex-0 took
	// the lease, started Plex, lost the lease twenty seconds later and logged
	// "lost the lease; restarting as a standby", then:
	//
	//	Plex is no longer serving; withdrawing this pod
	//	Plex has not answered since; giving up on this pod
	//	exiting so the pod restarts
	//
	// The watch must not outlive the leadership that started it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<MediaContainer machineIdentifier="test"/>`))
	}))
	t.Cleanup(srv.Close)

	m := newTestManager()
	m.plexAddr = srv.Listener.Addr().String()
	var restarts atomic.Int32
	m.onPlexLost = func(error) { restarts.Add(1) }

	const tick = 5 * time.Millisecond
	m.startHealthWatch(t.Context(), tick)
	time.Sleep(4 * tick)

	// What losing the lease does, in the order it does it.
	m.stopHealthWatch()
	srv.Close()

	time.Sleep(40 * tick)
	assert.Zero(t, restarts.Load(),
		"Plex was stopped deliberately, so the pod must stay up as a standby")
}
