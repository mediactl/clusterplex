package mediaproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func request(t *testing.T, target string, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestSessionKeyPrefersTheMostSpecificIdentifier(t *testing.T) {
	// A device can hold several playback sessions, so the session identifier
	// pins more precisely than the device one.
	r := request(t, "/library/metadata/1", map[string]string{
		"X-Plex-Session-Identifier": "session-1",
		"X-Plex-Client-Identifier":  "device-1",
	})
	assert.Equal(t, "session-1", SessionKey(r))
}

func TestSessionKeyFallsBackToTheDevice(t *testing.T) {
	r := request(t, "/library/metadata/1", map[string]string{"X-Plex-Client-Identifier": "device-1"})
	assert.Equal(t, "device-1", SessionKey(r))
}

func TestSessionKeyReadsQueryParametersToo(t *testing.T) {
	// Plex's own transcoder passes these on the query string rather than as
	// headers, so a transcode would otherwise land on an unrelated pod.
	r := request(t, "/video/:/transcode/universal/start?session=abc123", nil)
	assert.Equal(t, "abc123", SessionKey(r))
}

func TestAHeaderBeatsAQueryParameter(t *testing.T) {
	r := request(t, "/x?X-Plex-Client-Identifier=from-query", map[string]string{
		"X-Plex-Client-Identifier": "from-header",
	})
	assert.Equal(t, "from-header", SessionKey(r))
}

func TestSessionKeyIsEmptyWhenNothingIdentifiesTheClient(t *testing.T) {
	assert.Empty(t, SessionKey(request(t, "/identity", nil)))
}

func TestSessionKeyIgnoresBlankValues(t *testing.T) {
	r := request(t, "/x", map[string]string{"X-Plex-Client-Identifier": "   "})
	assert.Empty(t, SessionKey(r))
}
