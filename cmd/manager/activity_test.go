package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// activityPlex is a Plex serving two streams, one of them a transcode, a
// one-section library, and its version; anything else answers empty.
func activityPlex(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/status/sessions":
			_, _ = io.WriteString(w, `{"MediaContainer":{"Metadata":[
				{"ratingKey":"1","type":"movie","title":"Heat","User":{"title":"alice"},"Player":{"product":"Plex Web","platform":"Chrome","state":"playing"},
				 "Session":{"id":"a","bandwidth":8000,"location":"lan"},"TranscodeSession":{"videoDecision":"transcode","audioDecision":"copy","speed":2.1}},
				{"ratingKey":"2","type":"episode","title":"Pilot","User":{"title":"bob"},"Player":{"product":"Plex HTPC","platform":"Linux","state":"paused"},
				 "Session":{"id":"b","bandwidth":12000,"location":"lan"}}]}}`)
		case "/":
			_, _ = io.WriteString(w, `{"MediaContainer":{"version":"1.43.4.10389","platform":"Linux"}}`)
		case "/library/sections/all":
			_, _ = io.WriteString(w, `{"MediaContainer":{"Directory":[{"key":"1","type":"movie","title":"Movies"}]}}`)
		case "/library/sections/1/all":
			_, _ = io.WriteString(w, `{"MediaContainer":{"size":0,"totalSize":1200}}`)
		default:
			_, _ = io.WriteString(w, `{"MediaContainer":{}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func activityManager(t *testing.T, srv *httptest.Server) *Manager {
	t.Helper()
	m := newTestManager()
	m.Config.PlexDir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(m.Config.PlexDir, ".LocalAdminToken"), []byte("secret\n"), 0o600))
	m.plexAddr = strings.TrimPrefix(srv.URL, "http://")
	return m
}

func TestTheHealthWatchReportsWhatPlexIsServing(t *testing.T) {
	m := activityManager(t, activityPlex(t))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go m.watchPlexHealth(ctx, 5*time.Millisecond, time.Second)

	act := m.Metrics.Activity
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(m.Metrics.PlexSessions) == 2 &&
			testutil.CollectAndCount(act, "clusterplex_plex_streams") == 2 &&
			testutil.CollectAndCount(act, "clusterplex_plex_server_info") == 1
	}, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.Metrics.PlexTranscodeSessions))
	assert.Equal(t, 1, testutil.CollectAndCount(act, "clusterplex_plex_transcodes"))
}

func TestTheLeaseHolderCountsTheLibraryAndStopsWhenItLetsGo(t *testing.T) {
	m := activityManager(t, activityPlex(t))
	act := m.Metrics.Activity
	m.takePlexTV(t.Context())
	require.Eventually(t, func() bool { return testutil.CollectAndCount(act, "clusterplex_plex_library_items") == 1 },
		5*time.Second, 5*time.Millisecond)
	m.releasePlexTV(t.Context())
	require.Eventually(t, func() bool { return testutil.CollectAndCount(act, "clusterplex_plex_library_items") == 0 },
		5*time.Second, 5*time.Millisecond, "a pod that let the Lease go stops reporting the library, so the series never repeat")
}
