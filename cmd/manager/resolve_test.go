package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexdb "github.com/mediactl/clusterplex/pkg/plex/db"
)

// stubRow returns a fixed file path, standing in for the library database.
type stubRow struct{ file string }

func (s stubRow) Scan(dest ...any) error {
	if p, ok := dest[0].(*string); ok {
		*p = s.file
	}
	return nil
}

type stubQuerier struct{ row stubRow }

func (s stubQuerier) QueryRow(context.Context, string, ...any) plexdb.Row {
	return s.row
}

// fakePlex answers the authorization probe with a fixed status and records it.
func fakePlex(t *testing.T, status int, seen *http.Request) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = *r.Clone(context.Background())
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func resolve(t *testing.T, status int, token string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	var seen http.Request
	plex := fakePlex(t, status, &seen)
	h := &resolveHandler{
		PMSBase: plex.URL,
		DB:      &plexdb.DB{Querier: stubQuerier{row: stubRow{file: "/media/Movies/Test.mp4"}}},
	}
	req := httptest.NewRequest(http.MethodGet, "/internal/resolve?part=1&path=%2Flibrary%2Fparts%2F1%2F17%2Ffile.mp4", nil)
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr, &seen
}

func TestResolveAnswersWithTheFileWhenPlexAllowsTheRequest(t *testing.T) {
	rr, seen := resolve(t, http.StatusPartialContent, "good-token")

	require.Equal(t, http.StatusOK, rr.Code)
	var reply map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &reply))
	assert.Equal(t, "/media/Movies/Test.mp4", reply["file"])

	// Authorization is delegated, not reimplemented: the caller's credentials
	// are replayed against Plex, asking for a single byte.
	assert.Equal(t, "good-token", seen.Header.Get("X-Plex-Token"))
	assert.Equal(t, "bytes=0-0", seen.Header.Get("Range"))
	assert.Equal(t, "/library/parts/1/17/file.mp4", seen.URL.Path)
}

func TestResolveRefusesWhateverPlexRefuses(t *testing.T) {
	rr, _ := resolve(t, http.StatusUnauthorized, "bad-token")
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.NotContains(t, rr.Body.String(), "/media/", "a refused request must not leak the path")
}

func TestResolveRejectsAMalformedRequest(t *testing.T) {
	h := &resolveHandler{DB: &plexdb.DB{Querier: stubQuerier{}}}
	for _, q := range []string{"?part=1", "?path=/library/parts/1/1/file.mp4", "?part=1&path=relative"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/internal/resolve"+q, nil))
		assert.Equal(t, http.StatusBadRequest, rr.Code, "query %q", q)
	}
}
