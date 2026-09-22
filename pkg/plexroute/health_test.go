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

func TestAnsweringChecksTheLibraryWithTheLocalAdminToken(t *testing.T) {
	// /identity is served from memory, so a Plex whose database sessions are
	// all stuck still answers it instantly. Reading the library is the
	// request that hangs when the rest of Plex does, and it needs the token
	// or a claimed server refuses it before touching the database.
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/library/sections" {
			assert.Equal(t, "secret", r.Header.Get("X-Plex-Token"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	require.NoError(t, Answering(context.Background(), srv.URL, "secret"))
	assert.Equal(t, []string{"/identity", "/library/sections"}, paths)
}

func TestAServerThatAnswersIdentityButNotItsLibraryIsNotAnswering(t *testing.T) {
	// The failure this exists for: the listener is up, /identity is instant,
	// and every request that needs the database never completes. A watch on
	// /identity alone kept such a pod in service for seven minutes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := Answering(ctx, srv.URL, "secret")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deadline exceeded")
}

func TestAServerWhoseLibraryQueryFailsIsNotAnswering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := Answering(context.Background(), srv.URL, "secret")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestWithoutATokenAnsweringAsksOnlyWhatServingAsks(t *testing.T) {
	// A claimed server refuses an unauthenticated /library/sections before it
	// touches the database, which would prove nothing either way, and before
	// Plex is claimed there is no token at all.
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	require.NoError(t, Answering(context.Background(), srv.URL, ""))
	assert.Equal(t, []string{"/identity"}, paths)
}

func TestALibraryFailureIsMarkedAsOne(t *testing.T) {
	// The manager forgives library failures until the library has answered
	// once, so it has to be able to tell them from a listener that is gone.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := Answering(context.Background(), srv.URL, "secret")
	require.ErrorIs(t, err, ErrLibrary)
	assert.NotErrorIs(t, Serving(context.Background(), "http://127.0.0.1:1"), ErrLibrary,
		"a listener failure is not a library failure")
}
