// Package plexsession is what a relay does in a Plex transcode session on
// the serving pod: it writes the files a pool streams back into Plex's
// session directory, whole and under a name Plex's transcoder would use,
// and talks to Plex at its own address with the session's token. The remux
// relay and the video relay share it. It links no ffgo.
package plexsession

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
)

// WriteFile lands data in dir under name, which allowed must match: written
// to a temporary name and renamed, so Plex never reads part of a file.
func WriteFile(dir, name string, data []byte, allowed *regexp.Regexp) error {
	if !allowed.MatchString(name) || filepath.Base(name) != name {
		return fmt.Errorf("refusing file name %q", name)
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// PMS is this pod's Plex Media Server at its own address: a job's URLs name
// 127.0.0.1:32400, which from the manager's network namespace is the proxy.
type PMS struct {
	Addr string // "http://host:port"
	HTTP *http.Client
}

func (p PMS) client() *http.Client {
	if p.HTTP == nil {
		return http.DefaultClient
	}
	return p.HTTP
}

// at is raw on Plex's own address, with sub appended to its path
// ("stream", "streamDetail") and query, when not empty, replacing its own.
func (p PMS) at(raw, sub, query string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(p.Addr)
	if err != nil {
		return nil, err
	}
	u.Scheme, u.Host = base.Scheme, base.Host
	if sub != "" {
		u.Path += "/" + sub
	}
	if query != "" {
		u.RawQuery = query
	}
	return u, nil
}

// Send makes a request to Plex with the session's token and fails on any
// status from 300 up.
func (p PMS) Send(ctx context.Context, method, raw, sub, query string, body io.Reader, token string) error {
	u, err := p.at(raw, sub, query)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, u.Path, resp.Status)
	}
	return nil
}

// Open reads raw from Plex with the session's token: an input Plex serves
// itself, such as Live TV's session. The caller closes it.
func (p PMS) Open(ctx context.Context, raw, token string) (io.ReadCloser, error) {
	u, err := p.at(raw, "", "")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", u.Path, resp.Status)
	}
	return resp.Body, nil
}
