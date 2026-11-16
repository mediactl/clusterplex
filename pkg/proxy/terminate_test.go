package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testCert(t *testing.T, name string) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// A TLS client asking for a name the proxy holds a certificate for is ended
// at the proxy: its diverted request is answered by the divert, any other
// reaches the TLS upstream, and a name without a certificate is piped.
func TestTerminatedTLSOffersRequestsToTheDivert(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "plex:"+r.URL.Path)
	}))
	defer upstream.Close()
	cert := testCert(t, "a.b.plex.direct")
	p := &TCP{
		Listen: "127.0.0.1:0", Target: upstream.Listener.Addr().String(), Logger: slog.New(slog.DiscardHandler),
		Terminate: func(name string) *tls.Certificate {
			if name == "a.b.plex.direct" || name == "" {
				return cert
			}
			return nil
		},
		Divert: func(_ context.Context, req *http.Request) (*http.Response, bool) {
			if !strings.HasSuffix(req.URL.Path, "/hubs/discover") {
				return nil, false
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("diverted"))}, true
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, err := p.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	get := func(name, path string) (string, string) {
		t.Helper()
		var served string
		client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr.String())
			},
			TLSClientConfig: &tls.Config{ServerName: name, InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
				served = cs.PeerCertificates[0].Subject.CommonName
				return nil
			}},
		}}
		resp, err := client.Get("https://" + name + path)
		if err != nil {
			t.Fatalf("%s%s: %v", name, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return string(body), served
	}
	if body, served := get("a.b.plex.direct", "/tv.plex.providers.epg.xmltv:1/hubs/discover"); body != "diverted" || served != "a.b.plex.direct" {
		t.Errorf("diverted request: body %q, certificate %q", body, served)
	}
	if body, served := get("a.b.plex.direct", "/identity"); body != "plex:/identity" || served != "a.b.plex.direct" {
		t.Errorf("forwarded request: body %q, certificate %q", body, served)
	}
	// A client that names no server (it dialled an address) is ended too.
	conn, err := tls.Dial("tcp", addr.String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if cn := conn.ConnectionState().PeerCertificates[0].Subject.CommonName; cn != "a.b.plex.direct" {
		t.Errorf("no server name: certificate %q", cn)
	}
	_, _ = io.WriteString(conn, "GET /tv.plex.providers.epg.xmltv:1/hubs/discover HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	answer, _ := io.ReadAll(conn)
	_ = conn.Close()
	if !strings.HasSuffix(string(answer), "diverted") {
		t.Errorf("no server name: answer %q", answer)
	}
	if body, served := get("other.example", "/tv.plex.providers.epg.xmltv:1/hubs/discover"); !strings.HasPrefix(body, "plex:") || served == "a.b.plex.direct" {
		t.Errorf("a name without a certificate was not piped: body %q, certificate %q", body, served)
	}
}
