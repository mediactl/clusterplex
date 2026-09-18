package mediaproxy

import (
	"net/http"
	"strings"
)

// sessionHeaders identify a client, most specific first.
//
// Plex caches state in each server process's memory and there is no bus to
// invalidate it, so a client whose requests move between pods sees its own
// changes come and go. Pinning a client to one pod avoids that without trying
// to share memory. The session identifier is per playback session and the
// client identifier is per device; either keeps a client in one place, and the
// session one is preferred because it survives a device opening two of them.
var sessionHeaders = []string{
	"X-Plex-Session-Identifier",
	"X-Plex-Client-Identifier",
	"X-Plex-Device-Name",
}

// SessionKey returns a stable identifier for the client behind a request, or
// "" when it carries nothing to identify it.
//
// Plex clients send these as headers, but its own transcoder passes them as
// query parameters, so both are checked. The header wins when both are present.
func SessionKey(r *http.Request) string {
	for _, name := range sessionHeaders {
		if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
			return v
		}
	}
	query := r.URL.Query()
	for _, name := range append([]string{"session"}, sessionHeaders...) {
		if v := strings.TrimSpace(query.Get(name)); v != "" {
			return v
		}
	}
	return ""
}
