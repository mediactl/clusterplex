package epgcloud

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

// service answers with each status in turn, then 200.
func service(t *testing.T, statuses ...int) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := int(calls.Add(1)) - 1
		if n < len(statuses) {
			w.WriteHeader(statuses[n])
			return
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{}}`))
	}))
	t.Cleanup(srv.Close)
	old := retryBackoff
	retryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryBackoff = old })
	return &Client{BaseURL: srv.URL, Token: "t"}, &calls
}

func TestAFailedAnswerIsAskedAgainBeforeTheBuildGivesUp(t *testing.T) {
	c, calls := service(t, http.StatusServiceUnavailable, http.StatusBadGateway)
	body, err := c.Metadata(context.Background(), "x")
	require.NoError(t, err)
	assert.JSONEq(t, `{"MediaContainer":{}}`, string(body))
	assert.EqualValues(t, 3, calls.Load())

	c, calls = service(t, 503, 503, 503, 503)
	_, err = c.Metadata(context.Background(), "x")
	assert.ErrorContains(t, err, "HTTP 503")
	assert.EqualValues(t, 3, calls.Load(), "three tries, then the build fails and keeps the guide it has")
}

func TestARefusalOrAMissingAnswerIsNotAskedAgain(t *testing.T) {
	c, calls := service(t, http.StatusNotFound)
	_, err := c.Metadata(context.Background(), "x")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.EqualValues(t, 1, calls.Load())

	c, calls = service(t, http.StatusTooManyRequests)
	_, err = c.Metadata(context.Background(), "x")
	var rl *RateLimitedError
	assert.True(t, errors.As(err, &rl), "a 429 goes back to the pacer, which waits it out")
	assert.EqualValues(t, 1, calls.Load())
}
