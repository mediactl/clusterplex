package route

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// sessionsPath lists what Plex is serving right now.
const sessionsPath = "/status/sessions"

// maxSessionsBody bounds what is read from Plex; a session list is a few
// kilobytes per session.
const maxSessionsBody = 4 << 20

// Sessions is what one Plex is serving at a moment: every playing item, and
// how many of those it is transcoding. A pod's share of the cluster's load,
// and what a drain is waiting for.
type Sessions struct {
	Total       int
	Transcoding int
}

// ReadSessions asks Plex what it is serving. It needs the local admin token,
// like Answering; with none it reports an error rather than a guess.
func ReadSessions(ctx context.Context, baseURL, token string) (Sessions, error) {
	if token == "" {
		return Sessions{}, fmt.Errorf("reading sessions from %s needs a token", baseURL)
	}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	url := strings.TrimSuffix(baseURL, "/") + sessionsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Sessions{}, err
	}
	req.Header.Set("X-Plex-Token", token)
	req.Header.Set("Accept", "application/xml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Sessions{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Sessions{}, fmt.Errorf("plex at %s: %s %s", baseURL, sessionsPath, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSessionsBody))
	if err != nil {
		return Sessions{}, err
	}
	return parseSessions(body)
}

// parseSessions counts the playing items in a /status/sessions document and
// those with a TranscodeSession under them.
//
// Plex lists each item as an element named for its kind (Video, Track,
// Photo), so the count is of MediaContainer's children rather than of any
// one name; the size attribute is not trusted because it is what Plex says
// rather than what it lists.
func parseSessions(body []byte) (Sessions, error) {
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	var s Sessions
	depth := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Sessions{}, fmt.Errorf("parse %s: %w", sessionsPath, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 2:
				s.Total++
			case depth == 3 && t.Name.Local == "TranscodeSession":
				s.Transcoding++
			}
		case xml.EndElement:
			depth--
		}
	}
	return s, nil
}
