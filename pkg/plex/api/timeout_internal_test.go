package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestAHungPlexTimesOut: a Plex that accepts a connection and never answers
// must not hold the watcher's event handler for ever, so a Client without
// its own HTTP client gets one with a timeout.
func TestAHungPlexTimesOut(t *testing.T) {
	require.NotZero(t, defaultHTTP.Timeout)

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)

	saved := defaultHTTP
	defaultHTTP = &http.Client{Timeout: 50 * time.Millisecond}
	defer func() { defaultHTTP = saved }()

	start := time.Now()
	_, err := (&Client{BaseURL: srv.URL}).Sections(t.Context())
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second)
}
