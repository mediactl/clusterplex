// Package plexscan turns a library scan into a reconcile of clustarr's
// projection (spec 2026-10-15 §5, as amended by the capture doc of the same
// date): Plex 1.43 scans a library inside its own process, so the scan is
// caught as the HTTP request that asks for it, at the pod proxy, and the
// analyze invocation that still reaches the shim is read here too.
package plexscan

import (
	"net/http"
	"regexp"
)

// Kind is what the scan was asked to walk.
type Kind int

const (
	// KindSection is a whole library: Scan Library Files, the API without
	// a path, the maintenance refresh.
	KindSection Kind = iota + 1
	// KindFolder is one directory of a library: the API's path parameter.
	KindFolder
)

// Request is a library scan a client asked Plex for.
type Request struct {
	Section string
	Path    string
	Force   bool
	Kind    Kind
}

var scanPath = regexp.MustCompile(`^/library/sections/([0-9]+)/refresh$`)

// ParseScan reads a library-scan request. Plex Web and the API send
// POST /library/sections/{id}/refresh with path and force as query
// parameters or as a form body (curl --data-urlencode does the latter),
// and GET works too. Anything else is not a scan.
func ParseScan(r *http.Request) (Request, bool) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		return Request{}, false
	}
	m := scanPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return Request{}, false
	}
	_ = r.ParseForm() // the query plus a form body; a bad body leaves the query
	req := Request{Section: m[1], Path: r.Form.Get("path"), Force: r.Form.Get("force") == "1", Kind: KindSection}
	if req.Path != "" {
		req.Kind = KindFolder
	}
	return req, true
}
