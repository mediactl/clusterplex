package provision_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexprovision "github.com/mediactl/clusterplex/pkg/plex/provision"
)

func TestFetchRoot(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    plexprovision.Root
		wantErr error
	}{
		{"a provider", 200, `{"MediaProvider":{"identifier":"tv.plex.agents.custom.clustarr.movies","title":"Clustarr Movies","Types":[{"type":1}]}}`,
			plexprovision.Root{Identifier: "tv.plex.agents.custom.clustarr.movies", Title: "Clustarr Movies"}, nil},
		{"clustarr without --external-url", 503, `plex provider needs --external-url`, plexprovision.Root{}, plexprovision.ErrNotAProvider},
		{"JSON without an identifier", 200, `{"MediaProvider":{"title":"x"}}`, plexprovision.Root{}, plexprovision.ErrNotAProvider},
		{"an HTML page", 200, `<html>login</html>`, plexprovision.Root{}, plexprovision.ErrNotAProvider},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			got, err := plexprovision.FetchRoot(t.Context(), nil, srv.URL+"/plex/movies")
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
