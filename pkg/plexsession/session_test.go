package plexsession_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plexsession"
)

var hls = regexp.MustCompile(`^media-\d{5,}\.ts$`)

func TestWriteFileRefusesANameOutsideThePattern(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../x.ts", "x.ts", "media-1.ts", "media-00001.ts/../../y"} {
		require.Error(t, plexsession.WriteFile(dir, name, []byte("x"), hls), name)
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "a refused name left a file")
	_, err = os.Stat(filepath.Join(filepath.Dir(dir), "x.ts"))
	assert.True(t, os.IsNotExist(err))
}

func TestWriteFileLandsWholeUnderItsName(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, plexsession.WriteFile(dir, "media-00003.ts", []byte("segment"), hls))
	b, err := os.ReadFile(filepath.Join(dir, "media-00003.ts"))
	require.NoError(t, err)
	assert.Equal(t, "segment", string(b))
	entries, _ := os.ReadDir(dir)
	assert.Len(t, entries, 1, "the temporary file was left behind")
}

func TestPMSSendRewritesToPlexsOwnAddress(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	p := plexsession.PMS{Addr: srv.URL}
	err := p.Send(context.Background(), http.MethodPut,
		"http://127.0.0.1:32400/video/:/transcode/session/s/u/progress?ignored=1", "stream", "speed=2.0",
		strings.NewReader("x"), "tok")
	require.NoError(t, err)
	assert.Equal(t, "/video/:/transcode/session/s/u/progress/stream", got.URL.Path)
	assert.Equal(t, "speed=2.0", got.URL.RawQuery)
	assert.Equal(t, "tok", got.Header.Get("X-Plex-Token"))
	assert.Equal(t, "x", body)
}

func TestPMSOpenReadsAtPlexsOwnAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "tok" || r.URL.Path != "/livetv/sessions/abc/index.ts" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("live bytes"))
	}))
	defer srv.Close()
	p := plexsession.PMS{Addr: srv.URL}
	rc, err := p.Open(context.Background(), "http://127.0.0.1:32400/livetv/sessions/abc/index.ts", "tok")
	require.NoError(t, err)
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, "live bytes", string(b))

	_, err = p.Open(context.Background(), "http://127.0.0.1:32400/livetv/sessions/abc/index.ts", "wrong")
	require.ErrorContains(t, err, "403")
}
