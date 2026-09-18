package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func trigger(t *testing.T, handler http.HandlerFunc) (int, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	var out, errOut bytes.Buffer
	code := run(context.Background(), srv.URL, "analyze", 5*time.Second, &out, &errOut)
	return code, out.String() + errOut.String()
}

func TestSucceedsWhenTheManagerAcceptsTheTask(t *testing.T) {
	code, output := trigger(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/maintenance/analyze", r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"dispatched":3}`))
	})

	assert.Zero(t, code)
	assert.Contains(t, output, "dispatched")
}

func TestFailsWhenTheManagerRefuses(t *testing.T) {
	// A failed Job is the visible signal; succeeding here would hide it.
	code, output := trigger(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("no pod is running Plex"))
	})

	assert.NotZero(t, code)
	assert.Contains(t, output, "no pod is running Plex")
}

func TestFailsWhenTheTaskIsUnknown(t *testing.T) {
	code, _ := trigger(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	assert.NotZero(t, code)
}

func TestFailsWhenNoManagerAnswers(t *testing.T) {
	// This is the failover case: the Job should fail and be retried, by which
	// time another pod has taken over.
	var out, errOut bytes.Buffer
	code := run(context.Background(), "http://127.0.0.1:1", "analyze", 200*time.Millisecond, &out, &errOut)

	assert.NotZero(t, code)
	assert.Contains(t, errOut.String(), "analyze")
}

func TestTrailingSlashesOnTheEndpointDoNotMatter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/maintenance/analyze", r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := run(context.Background(), srv.URL+"/", "analyze", 5*time.Second, &out, &errOut)
	assert.Zero(t, code)
}
