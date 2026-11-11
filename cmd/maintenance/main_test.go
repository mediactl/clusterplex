package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mediactl/clusterplex/pkg/logging"
	"github.com/stretchr/testify/require"

	"github.com/stretchr/testify/assert"

	"github.com/mediactl/clusterplex/pkg/maintenance"
)

func trigger(t *testing.T, handler http.HandlerFunc) (int, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	var out bytes.Buffer
	_, log, err := newRoot(t.Context(), logging.Options{Format: "json"}, &out)
	require.NoError(t, err)
	code := run(context.Background(), log, srv.URL, "analyze", 5*time.Second)
	return code, out.String()
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
	var out bytes.Buffer
	_, log, err := newRoot(t.Context(), logging.Options{Format: "json"}, &out)
	require.NoError(t, err)
	code := run(context.Background(), log, "http://127.0.0.1:1", "analyze", 200*time.Millisecond)

	assert.NotZero(t, code)
	assert.Contains(t, out.String(), "analyze")
}

func TestTrailingSlashesOnTheEndpointDoNotMatter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/maintenance/analyze", r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	var out bytes.Buffer
	_, log, err := newRoot(t.Context(), logging.Options{Format: "json"}, &out)
	require.NoError(t, err)
	code := run(context.Background(), log, srv.URL+"/", "analyze", 5*time.Second)
	assert.Zero(t, code)
}

func TestTheTriggerOutwaitsTheManager(t *testing.T) {
	// A trigger that gives up first fails the Job while the manager is still
	// working, and the retry asks Plex for the same work again.
	for _, name := range maintenance.Names() {
		task, _ := maintenance.Lookup(name)
		assert.Greater(t, waitFor(name, 0), task.Deadline(), "task %s", name)
	}
}

func TestAnExplicitTimeoutIsKept(t *testing.T) {
	assert.Equal(t, 5*time.Second, waitFor("empty-trash", 5*time.Second))
}

func TestAnUnknownTaskWaitsAMinute(t *testing.T) {
	// The manager answers 404 at once; there is nothing to wait for.
	assert.Equal(t, time.Minute, waitFor("no-such-task", 0))
}

// The process's first record carries its service (the 2026-11-10
// structured-logging design §3): service.name maintenance under cluster-plex.
func TestItsFirstLineCarriesItsService(t *testing.T) {
	var out bytes.Buffer
	_, log, err := newRoot(t.Context(), logging.Options{Format: "json"}, &out)
	require.NoError(t, err)
	log.Info("started")
	line, _, _ := bytes.Cut(out.Bytes(), []byte("\n"))
	var rec struct {
		Service struct{ Name, Namespace string } `json:"service"`
	}
	require.NoError(t, json.Unmarshal(line, &rec), "%s", out.String())
	assert.Equal(t, "maintenance", rec.Service.Name)
	assert.Equal(t, "cluster-plex", rec.Service.Namespace)
}
