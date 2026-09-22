package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTheHealthWatchRestartsThePodWhenPlexStopsAnsweringForItsLibrary(t *testing.T) {
	// The other way Plex dies. Its listener stays up and /identity, served
	// from memory, keeps answering in milliseconds, while every request that
	// needs the database never completes. Observed as thirty threads
	// "waiting on db connections" and seven minutes of failed playback on a
	// pod the watch called healthy the whole time.
	var libraryHangs atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" && libraryHangs.Load() {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	m := newTestManager()
	m.Config.PlexDir = t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(m.Config.PlexDir, ".LocalAdminToken"), []byte("secret\n"), 0o600))
	m.plexAddr = srv.Listener.Addr().String()
	var restarts atomic.Int32
	m.onPlexLost = func(error) { restarts.Add(1) }

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const tick = 5 * time.Millisecond
	go m.watchPlexHealth(ctx, tick, 20*time.Millisecond)

	// Nothing happens while Plex answers for its library.
	time.Sleep(10 * tick)
	require.Zero(t, restarts.Load(), "a Plex that reads its library must never restart the pod")

	libraryHangs.Store(true)

	assert.Eventually(t, func() bool { return restarts.Load() == 1 },
		2*time.Second, tick,
		"the pod is given up on once Plex stops answering for its library")
}

func TestAPlexStillMigratingIsNotHeldToItsLibrary(t *testing.T) {
	// The watch starts as soon as /identity answers, and Plex answers that
	// before it has finished with its database: a version upgrade spends
	// minutes migrating and rebuilding the search index. Counting those
	// minutes as misses would restart Plex mid-migration — and a restart
	// that is retried would never let it finish. Until the library has
	// answered once, only /identity counts.
	var libraryReady atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" && !libraryReady.Load() {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	m := newTestManager()
	m.Config.PlexDir = t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(m.Config.PlexDir, ".LocalAdminToken"), []byte("secret\n"), 0o600))
	m.plexAddr = srv.Listener.Addr().String()
	var restarts atomic.Int32
	m.onPlexLost = func(error) { restarts.Add(1) }

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const tick = 5 * time.Millisecond
	go m.watchPlexHealth(ctx, tick, 20*time.Millisecond)

	// Far more than three failed library checks pass while it migrates.
	time.Sleep(40 * tick)
	require.Zero(t, restarts.Load(), "a Plex that has not finished starting must not be restarted for its library")

	// Once the library has answered, a later hang is held against it.
	libraryReady.Store(true)
	time.Sleep(10 * tick)
	libraryReady.Store(false)
	assert.Eventually(t, func() bool { return restarts.Load() == 1 },
		2*time.Second, tick,
		"after the library has answered once, its silence is a failure")
}

func TestAPlexWhoseListenerDiesWhileMigratingIsStillGivenUpOn(t *testing.T) {
	// The startup grace covers the library only. A listener that goes away
	// before the library ever answered is Plex gone, exactly as before.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	m := newTestManager()
	m.Config.PlexDir = t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(m.Config.PlexDir, ".LocalAdminToken"), []byte("secret\n"), 0o600))
	m.plexAddr = srv.Listener.Addr().String()
	var restarts atomic.Int32
	m.onPlexLost = func(error) { restarts.Add(1) }

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const tick = 5 * time.Millisecond
	go m.watchPlexHealth(ctx, tick, 20*time.Millisecond)
	time.Sleep(10 * tick)
	require.Zero(t, restarts.Load())

	srv.Close()

	assert.Eventually(t, func() bool { return restarts.Load() == 1 },
		2*time.Second, tick, "a dead listener is a dead Plex, migrating or not")
}

func TestTheLibraryGraceStartsOverForEachLifeOfPlex(t *testing.T) {
	// A watch outlives a Plex that is restarted underneath it, and the one
	// that comes back has the same migrating to do. If the grace were spent
	// by the first life, the second would be restarted mid-migration -- and
	// with restarts retried, every life after that too.
	var (
		listenerUp   atomic.Bool
		libraryReady atomic.Bool
	)
	listenerUp.Store(true)
	libraryReady.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !listenerUp.Load() {
			w.WriteHeader(http.StatusServiceUnavailable) // Plex holds the port but is gone
			return
		}
		if r.URL.Path == "/library/sections" && !libraryReady.Load() {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	m := newTestManager()
	m.Config.PlexDir = t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(m.Config.PlexDir, ".LocalAdminToken"), []byte("secret\n"), 0o600))
	m.plexAddr = srv.Listener.Addr().String()
	var restarts atomic.Int32
	m.onPlexLost = func(error) { restarts.Add(1) }

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const tick = 5 * time.Millisecond
	go m.watchPlexHealth(ctx, tick, 20*time.Millisecond)

	// First life: healthy, library answering, grace spent.
	time.Sleep(10 * tick)
	require.Zero(t, restarts.Load())

	// Plex dies; the watch gives up on it once.
	listenerUp.Store(false)
	require.Eventually(t, func() bool { return restarts.Load() == 1 }, 2*time.Second, tick)

	// Second life comes back migrating: listener up, library silent.
	libraryReady.Store(false)
	listenerUp.Store(true)
	time.Sleep(40 * tick)
	assert.Equal(t, int32(1), restarts.Load(),
		"the new life of Plex gets its startup grace; it must not be restarted for a library it has not finished with")

	// And once this life has answered, its silence counts again.
	libraryReady.Store(true)
	time.Sleep(10 * tick)
	libraryReady.Store(false)
	assert.Eventually(t, func() bool { return restarts.Load() == 2 }, 2*time.Second, tick)
}
