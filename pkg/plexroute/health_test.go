package plexroute

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAServerThatAnswersIsServing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/identity", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	assert.NoError(t, Serving(context.Background(), srv.URL))
}

func TestAServerThatAcceptsButRefusesEveryRequestIsNotServing(t *testing.T) {
	// This is the state that matters. Plex binds its port early and answers
	// 503 to everything, including its own requests, until it has finished
	// starting — and it can die part way through and stay in that state. A
	// check that only dials the port calls that healthy, so the pod goes on
	// claiming it can serve while serving nothing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := Serving(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

func TestNothingListeningIsNotServing(t *testing.T) {
	assert.Error(t, Serving(context.Background(), "http://127.0.0.1:1"))
}

func TestAnAuthenticationChallengeCountsAsServing(t *testing.T) {
	// A claimed server refuses an unauthenticated /identity. That is a
	// working server declining the caller, not a broken one.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	assert.NoError(t, Serving(context.Background(), srv.URL))
}

func TestWaitServingGivesUpAtTheDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := WaitServing(context.Background(), srv.URL, 300*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

func TestWaitServingReturnsOnceTheServerComesUp(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	require.NoError(t, WaitServing(context.Background(), srv.URL, 5*time.Second))
	assert.GreaterOrEqual(t, calls, 3)
}
