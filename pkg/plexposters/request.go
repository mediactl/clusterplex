// Package plexposters is the proxy's view of the posters clients ask Plex
// for (clustarr's compact-posters design, 2026-11-27, §5.4): a client asks
// PMS's photo transcoder for an item's poster at a size, and a small one is
// to be answered with clustarr's compact rendition, whose badges and title
// image are drawn larger.
//
// What is here is the first step, observe: every request goes on to Plex
// as it came, and the proxy counts what was asked -- by which platform, at
// what width, and what the request would have met -- so the rules that say
// which requests are small are written from what clients really send.
package plexposters

import (
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// transcodePath is the photo transcoder's path.
const transcodePath = "/photo/:/transcode"

// What the image a request asks for is.
const (
	// TargetPoster is an item's own poster: /library/metadata/<id>/thumb.
	TargetPoster = "poster"
	// TargetLocal is any other image Plex itself holds.
	TargetLocal = "local"
	// TargetPlexCloud is an image on Plex's cloud photo service.
	TargetPlexCloud = "plex-cloud"
	// TargetPublic is an image on any other host.
	TargetPublic = "public"
	// TargetNone is a request that names no image.
	TargetNone = "none"
)

// plexCloudHost is Plex's cloud photo service.
const plexCloudHost = "images.plex.tv"

var posterPath = regexp.MustCompile(`^/library/metadata/([0-9]+)/thumb(?:/[0-9]+)?$`)

// Request is what a photo transcode request says about itself. It holds no
// token and nothing that names an item to a reader but Item, which is never
// logged or made a label.
type Request struct {
	// Platform and Product are the client's X-Plex-Platform and
	// X-Plex-Product; PlatformIn is where the platform travelled, "header"
	// or "query", empty when it was not sent.
	Platform, Product, PlatformIn string
	// TokenIn is where the client's token travelled, "header" or "query",
	// empty when the request carried none.
	TokenIn string
	// Width and Height are the pixels asked for, 0 when not a number.
	Width, Height int
	// MinSize and Upscale are those parameters as sent.
	MinSize, Upscale string
	// Item is the row whose poster is asked for (Target is TargetPoster).
	Item int64
	// Target is what the image asked for is.
	Target string
	// TLS is a request whose TLS the proxy ended.
	TLS bool
	// Params are the names of the query's parameters, sorted.
	Params []string
}

// Parse reads a request to the photo transcoder; false for any other.
func Parse(req *http.Request) (Request, bool) {
	if req.Method != http.MethodGet || req.URL.Path != transcodePath {
		return Request{}, false
	}
	q := req.URL.Query()
	r := Request{
		Product: firstOf(req.Header.Get("X-Plex-Product"), q.Get("X-Plex-Product")),
		MinSize: q.Get("minSize"),
		Upscale: q.Get("upscale"),
		TLS:     req.TLS != nil,
	}
	r.Width, _ = strconv.Atoi(q.Get("width"))
	r.Height, _ = strconv.Atoi(q.Get("height"))
	switch {
	case req.Header.Get("X-Plex-Platform") != "":
		r.Platform, r.PlatformIn = req.Header.Get("X-Plex-Platform"), "header"
	case q.Get("X-Plex-Platform") != "":
		r.Platform, r.PlatformIn = q.Get("X-Plex-Platform"), "query"
	}
	switch {
	case req.Header.Get("X-Plex-Token") != "":
		r.TokenIn = "header"
	case q.Get("X-Plex-Token") != "":
		r.TokenIn = "query"
	}
	for name := range q {
		r.Params = append(r.Params, name)
	}
	slices.Sort(r.Params)
	r.Target, r.Item = target(q.Get("url"))
	return r, true
}

// target says what the image at raw, a transcode request's url, is, and
// the row when it is an item's poster.
func target(raw string) (string, int64) {
	if raw == "" {
		return TargetNone, 0
	}
	u, err := url.Parse(raw)
	if err != nil {
		return TargetNone, 0
	}
	switch host := strings.ToLower(u.Hostname()); {
	case host == plexCloudHost:
		return TargetPlexCloud, 0
	case host != "" && host != "127.0.0.1" && host != "localhost" && host != "::1":
		return TargetPublic, 0
	}
	if m := posterPath.FindStringSubmatch(u.Path); m != nil {
		if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return TargetPoster, id
		}
	}
	return TargetLocal, 0
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
