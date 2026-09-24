package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const twoSessionsOneTranscoding = `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="2">
<Video ratingKey="1" title="A" type="movie">
<Media id="1"><Part id="1"/></Media>
<User id="1" title="x"/><Player address="10.0.0.1" state="playing"/>
<TranscodeSession key="/transcode/sessions/abc" videoDecision="transcode" audioDecision="copy"/>
</Video>
<Track ratingKey="2" title="B" type="track">
<Media id="2"><Part id="2"/></Media>
<User id="1" title="x"/><Player address="10.0.0.2" state="playing"/>
</Track>
</MediaContainer>`

func TestParseSessionsCountsItemsAndTranscodes(t *testing.T) {
	s, err := parseSessions([]byte(twoSessionsOneTranscoding))
	require.NoError(t, err)
	assert.Equal(t, Sessions{Total: 2, Transcoding: 1}, s)
}

func TestParseSessionsOfAnIdleServerIsZero(t *testing.T) {
	s, err := parseSessions([]byte(`<?xml version="1.0" encoding="UTF-8"?><MediaContainer size="0"></MediaContainer>`))
	require.NoError(t, err)
	assert.Equal(t, Sessions{}, s)
}

func TestParseSessionsDoesNotTrustTheSizeAttribute(t *testing.T) {
	// size is what Plex says; the children are what it lists.
	s, err := parseSessions([]byte(`<MediaContainer size="7"><Video ratingKey="1"/></MediaContainer>`))
	require.NoError(t, err)
	assert.Equal(t, 1, s.Total)
}

func TestParseSessionsRejectsGarbage(t *testing.T) {
	_, err := parseSessions([]byte(`<MediaContainer><Video>`))
	assert.Error(t, err)
}

func TestReadSessionsSendsTheTokenAndCounts(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Plex-Token")
		if r.URL.Path != sessionsPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(twoSessionsOneTranscoding))
	}))
	defer srv.Close()
	s, err := ReadSessions(context.Background(), srv.URL, "tok")
	require.NoError(t, err)
	assert.Equal(t, "tok", gotToken)
	assert.Equal(t, Sessions{Total: 2, Transcoding: 1}, s)
}

func TestReadSessionsNeedsAToken(t *testing.T) {
	_, err := ReadSessions(context.Background(), "http://127.0.0.1:1", "")
	assert.Error(t, err, "an unauthenticated read would be refused before Plex looked; say so instead of guessing zero")
}

func TestReadSessionsReportsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := ReadSessions(context.Background(), srv.URL, "bad")
	assert.Error(t, err)
}
