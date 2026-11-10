package collections

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/mediactl/clusterplex/pkg/plex/api"
)

// The image fetch's limits (the design's Presentation).
const (
	fetchTimeout   = 30 * time.Second
	maxRedirects   = 5
	maxImageFetch  = api.MaxImageBytes
	imageTypeStart = "image/"
)

var errRefusedTarget = errors.New("not an allowed image address")

// NewImageFetcher fetches a poster or background for upload. It takes an
// https URL whose every dialled address is public -- checked as each
// connection is made, so neither a redirect nor a DNS answer reaches a
// private one -- or any URL under uiArtBase, clustarr UI's own /art/ (its
// external URL plus /art/), which may be in-cluster. An empty uiArtBase
// refuses those. A body over 10 MiB, or of a type not image/*, is refused.
func NewImageFetcher(uiArtBase string) ImageFetcher {
	return newImageFetcher(uiArtBase, publicAddress, nil)
}

// newImageFetcher is NewImageFetcher with the dial check and the trusted
// roots given, for tests against httptest servers.
func newImageFetcher(uiArtBase string, public func(address string) bool, roots *x509.CertPool) ImageFetcher {
	var art *url.URL
	if uiArtBase != "" {
		if u, err := url.Parse(uiArtBase); err == nil && u.Host != "" {
			art = u
		}
	}
	underArt := func(u *url.URL) bool {
		if art == nil || u.Scheme != art.Scheme || u.Host != art.Host {
			return false
		}
		p := path.Clean("/" + u.Path)
		return strings.HasPrefix(p+"/", strings.TrimSuffix(art.Path, "/")+"/") && p != path.Clean("/"+art.Path)
	}
	artAddr := ""
	if art != nil {
		artAddr = hostPort(art)
	}
	// The ui's host is dialled without the public check, so on that host
	// only /art/ is allowed, whatever the scheme.
	allowed := func(u *url.URL) error {
		switch {
		case underArt(u):
			return nil
		case artAddr != "" && hostPort(u) == artAddr:
			return fmt.Errorf("%w: %s is on the ui's host but not under its /art/", errRefusedTarget, u.Redacted())
		case u.Scheme == "https":
			return nil
		}
		return fmt.Errorf("%w: %s is neither https nor under the ui's /art/", errRefusedTarget, u.Redacted())
	}

	checked := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		if !public(address) {
			return fmt.Errorf("%w: %s is not a public address", errRefusedTarget, address)
		}
		return nil
	}}
	plain := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		// The ui's own host may be private; every other host is dialled
		// through the public check.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if artAddr != "" && addr == artAddr {
				return plain.DialContext(ctx, network, addr)
			}
			return checked.DialContext(ctx, network, addr)
		},
		TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: fetchTimeout,
	}
	client := &http.Client{
		Timeout:   fetchTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("more than %d redirects", maxRedirects)
			}
			return allowed(req.URL)
		},
	}

	return func(ctx context.Context, raw string) ([]byte, string, error) {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, "", fmt.Errorf("image URL: %w", err)
		}
		if err := allowed(u); err != nil {
			return nil, "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Accept", "image/*")
		resp, err := client.Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("fetch %s: %w", u.Redacted(), err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("fetch %s: %s", u.Redacted(), resp.Status)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, imageTypeStart) {
			return nil, "", fmt.Errorf("fetch %s: %q is not an image", u.Redacted(), ct)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageFetch+1))
		if err != nil {
			return nil, "", fmt.Errorf("fetch %s: %w", u.Redacted(), err)
		}
		if len(body) > maxImageFetch {
			return nil, "", fmt.Errorf("fetch %s: over %d bytes", u.Redacted(), maxImageFetch)
		}
		return body, ct, nil
	}
}

// hostPort is a URL's host and port as the transport dials it.
func hostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "http" {
		return net.JoinHostPort(u.Hostname(), "80")
	}
	return net.JoinHostPort(u.Hostname(), "443")
}

// notPublic are ranges IsGlobalUnicast admits that are no public host:
// "this network", shared (CGNAT), IETF protocol assignments, the
// documentation and benchmarking nets, future use, IPv6 documentation,
// local-use NAT64 and Teredo (whose IPv4 is obfuscated).
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
}

// Translated IPv6 carries an IPv4 address, which decides: NAT64's well-
// known prefix holds it in the last four bytes, 6to4 in bytes 2 to 5.
var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
)

// publicAddress says whether a dialled "ip:port" is a public unicast
// address: not private, loopback, link-local, unspecified, shared,
// reserved or documentation, and for translated IPv6 its IPv4 likewise.
func publicAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return publicIP(ip.Unmap())
}

func publicIP(ip netip.Addr) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(ip) {
			return false
		}
	}
	b := ip.As16()
	switch {
	case nat64.Contains(ip):
		return publicIP(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	case sixToFour.Contains(ip):
		return publicIP(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
	}
	return true
}
