package plexscan

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Plex Web and the API send the scan as POST /library/sections/{id}/refresh,
// with path and force in the query or in a form body, and GET works too.
func TestParseScanReadsSectionPathAndForceFromQueryOrBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *http.Request
		want Request
	}{
		{"section, no path", httptest.NewRequest(http.MethodPost, "/library/sections/2/refresh", nil), Request{Section: "2", Kind: KindSection}},
		{"path in query", httptest.NewRequest(http.MethodPost, "/library/sections/1/refresh?path=%2Fmedia%2Fmovies%2FHeat+%281995%29&force=1", nil), Request{Section: "1", Path: "/media/movies/Heat (1995)", Force: true, Kind: KindFolder}},
		{"path in body", formPost("/library/sections/1/refresh", "path=%2Fmedia%2Ftv%2FShow"), Request{Section: "1", Path: "/media/tv/Show", Kind: KindFolder}},
		{"GET is accepted too", httptest.NewRequest(http.MethodGet, "/library/sections/2/refresh?force=1", nil), Request{Section: "2", Force: true, Kind: KindSection}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseScan(tc.req)
			require.True(t, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

func formPost(target, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

// Anything that is not a library scan passes to Plex untouched: an item
// refresh, a listing, an analyze, a section delete.
func TestParseScanLeavesEveryOtherRequestAlone(t *testing.T) {
	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodPut, "/library/metadata/7/refresh", nil),
		httptest.NewRequest(http.MethodGet, "/library/sections/2/all", nil),
		httptest.NewRequest(http.MethodPost, "/library/sections/2/analyze", nil),
		httptest.NewRequest(http.MethodGet, "/library/sections", nil),
		httptest.NewRequest(http.MethodDelete, "/library/sections/2/refresh", nil),
	} {
		_, ok := ParseScan(r)
		require.False(t, ok, r.Method+" "+r.URL.String())
	}
}
