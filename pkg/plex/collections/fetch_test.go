package collections_test

import (
	"bytes"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/collections"
)

// imageServer answers /ok.png with an image, /big.png with 11 MiB,
// /page with HTML, /loop by redirecting to itself and /to?u= by
// redirecting to u.
func imageServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png", "/art/poster.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("\x89PNG"))
		case "/big.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(bytes.Repeat([]byte{0}, 11<<20))
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>"))
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/to":
			http.Redirect(w, r, r.URL.Query().Get("u"), http.StatusFound)
		default:
			// Any path ending /settings is served, so a refusal of one is
			// the fetcher's, not a 404's.
			if strings.HasSuffix(r.URL.Path, "/settings") {
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write([]byte("\x89PNG"))
				return
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func roots(srvs ...*httptest.Server) *x509.CertPool {
	p := x509.NewCertPool()
	for _, s := range srvs {
		p.AddCert(s.Certificate())
	}
	return p
}

func TestFetcherCaps(t *testing.T) {
	srv := imageServer(t)
	fetch := collections.NewImageFetcherWith("", func(string) bool { return true }, roots(srv))

	body, ct, err := fetch(t.Context(), srv.URL+"/ok.png")
	require.NoError(t, err)
	assert.Equal(t, "image/png", ct)
	assert.Equal(t, []byte("\x89PNG"), body)

	for _, path := range []string{"/big.png", "/page", "/loop", "/nope"} {
		_, _, err := fetch(t.Context(), srv.URL+path)
		assert.Error(t, err, path)
	}
}

func TestFetcherRefusesPrivateTargets(t *testing.T) {
	public := imageServer(t)
	private := imageServer(t)
	publicAddr := strings.TrimPrefix(public.URL, "https://")
	// Only the "public" server's address passes the dial check.
	isPublic := func(address string) bool { return address == publicAddr }
	certs := roots(public, private)

	fetch := collections.NewImageFetcherWith("", isPublic, certs)
	_, _, err := fetch(t.Context(), public.URL+"/ok.png")
	require.NoError(t, err)
	_, _, err = fetch(t.Context(), private.URL+"/ok.png")
	assert.Error(t, err, "a private address directly")
	_, _, err = fetch(t.Context(), public.URL+"/to?u="+private.URL+"/ok.png")
	assert.Error(t, err, "a private address through a redirect")
	_, _, err = fetch(t.Context(), "http://"+publicAddr+"/ok.png")
	assert.Error(t, err, "plain http off the ui's /art/")

	// The ui's own /art/ may be in-cluster, but only /art/.
	artBase := private.URL + "/art/"
	fetch = collections.NewImageFetcherWith(artBase, isPublic, certs)
	_, _, err = fetch(t.Context(), private.URL+"/art/poster.png")
	require.NoError(t, err, "under the ui's /art/")
	_, _, err = fetch(t.Context(), public.URL+"/to?u="+private.URL+"/art/poster.png")
	require.NoError(t, err, "a redirect into the ui's /art/")
	_, _, err = fetch(t.Context(), private.URL+"/settings")
	assert.Error(t, err, "the ui's host off /art/")
	_, _, err = fetch(t.Context(), private.URL+"/art/../settings")
	assert.Error(t, err, "a path escaping /art/")
}

func TestPublicAddress(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.216.34:443":  true,
		"[2606:4700::1]:443": true,
		"127.0.0.1:443":      false,
		"10.1.1.102:443":     false,
		"192.168.1.5:443":    false,
		"172.16.0.1:443":     false,
		"169.254.169.254:80": false,
		"100.64.0.1:443":     false,
		"[::1]:443":          false,
		"[fe80::1]:443":      false,
		"[fd00::1]:443":      false,
		"0.0.0.0:443":        false,
		"not-an-ip:443":      false,
		// Reserved, benchmarking, documentation and future-use ranges.
		"0.1.2.3:443":       false,
		"192.0.0.8:443":     false,
		"192.0.2.10:443":    false,
		"198.18.0.1:443":    false,
		"198.51.100.7:443":  false,
		"203.0.113.9:443":   false,
		"240.0.0.1:443":     false,
		"[2001:db8::1]:443": false,
		// Translated IPv6 is judged by the IPv4 inside it.
		"[64:ff9b::a00:1]:443":                       false, // NAT64 of 10.0.0.1
		"[64:ff9b::5db8:d822]:443":                   true,  // NAT64 of 93.184.216.34
		"[64:ff9b:1::a00:1]:443":                     false, // local-use NAT64
		"[2002:a00:1::1]:443":                        false, // 6to4 of 10.0.0.1
		"[2002:5db8:d822::1]:443":                    true,  // 6to4 of 93.184.216.34
		"[2001:0:4136:e378:8000:63bf:3fff:fdd2]:443": false, // Teredo
	} {
		assert.Equal(t, want, collections.PublicAddress(addr), addr)
	}
}
