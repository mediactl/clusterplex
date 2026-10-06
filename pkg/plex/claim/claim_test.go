package claim

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exchanged is plex.tv's answer to a claim exchange, the shape Plex's own
// image reads (pms-docker's 40-plex-first-run takes <authentication-token>).
const exchanged = `<?xml version="1.0" encoding="UTF-8"?>
<user email="owner@example.com" id="1" username="owner" authenticationToken="server-token">
  <subscription active="1" status="Active" plan="lifetime"/>
  <authentication-token>server-token</authentication-token>
</user>`

func TestExchangeTradesTheClaimForTheServersToken(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte(exchanged))
	}))
	defer srv.Close()

	token, err := Exchanger{BaseURL: srv.URL}.Exchange(context.Background(), "claim-abc", "25648e79229b88b46fdb829e0bdabedf7c385304")
	require.NoError(t, err)
	assert.Equal(t, "server-token", token)

	require.NotNil(t, got)
	assert.Equal(t, http.MethodPost, got.Method)
	assert.Equal(t, "/api/claim/exchange", got.URL.Path)
	assert.Equal(t, "claim-abc", got.URL.Query().Get("token"))
	// The server claims as itself: plex.tv files the token under the
	// identity clients see.
	assert.Equal(t, "25648e79229b88b46fdb829e0bdabedf7c385304", got.Header.Get("X-Plex-Client-Identifier"))
	assert.Equal(t, "Plex Media Server", got.Header.Get("X-Plex-Product"))
	assert.Equal(t, "server", got.Header.Get("X-Plex-Provides"))
	assert.Equal(t, "Linux", got.Header.Get("X-Plex-Platform"))
}

func TestExchangeReadsTheTokenAttributeWhenThereIsNoElement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<user id="1" authToken="server-token"/>`))
	}))
	defer srv.Close()
	token, err := Exchanger{BaseURL: srv.URL}.Exchange(context.Background(), "claim-abc", "id")
	require.NoError(t, err)
	assert.Equal(t, "server-token", token)
}

func TestExchangeReportsAnAnswerWithoutAToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<user id="1"/>`))
	}))
	defer srv.Close()
	_, err := Exchanger{BaseURL: srv.URL}.Exchange(context.Background(), "claim-abc", "id")
	require.ErrorIs(t, err, ErrNoToken)
}

// An expired or spent claim code is refused; the error names the status and
// never the code, which is a credential for the next four minutes.
func TestExchangeReportsARefusalWithoutTheClaim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "<errors><error>Invalid token claim-abc</error></errors>", http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := Exchanger{BaseURL: srv.URL}.Exchange(context.Background(), "claim-abc", "id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.NotContains(t, err.Error(), "claim-abc")
}

func TestExchangeKeepsTheClaimOutOfATransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // refused
	_, err := Exchanger{BaseURL: srv.URL}.Exchange(context.Background(), "claim-abc", "id")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "claim-abc")
}

func TestExchangeCapsTheAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<user authToken="x">` + strings.Repeat("a", maxBody+1) + `</user>`))
	}))
	defer srv.Close()
	_, err := Exchanger{BaseURL: srv.URL}.Exchange(context.Background(), "claim-abc", "id")
	require.ErrorIs(t, err, ErrTooLarge)
}

func TestExchangeRefusesAnEmptyClaimOrIdentity(t *testing.T) {
	_, err := Exchanger{BaseURL: "http://unused.invalid"}.Exchange(context.Background(), "", "id")
	require.Error(t, err)
	_, err = Exchanger{BaseURL: "http://unused.invalid"}.Exchange(context.Background(), "claim-abc", "")
	require.Error(t, err)
}
