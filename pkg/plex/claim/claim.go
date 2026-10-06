// Package claim trades a plex.tv claim code for the server's own token,
// PlexOnlineToken, as Plex's official image does with PLEX_CLAIM before Plex
// first starts (pms-docker, root/etc/cont-init.d/40-plex-first-run).
//
// A claim code comes from https://plex.tv/claim, lasts about four minutes and
// works once. What it is exchanged for is the long-lived credential, so
// neither ever appears in an error or a log line.
package claim

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// DefaultBaseURL is plex.tv.
const DefaultBaseURL = "https://plex.tv"

// maxBody caps the answer: a user document is a few kilobytes.
const maxBody = 1 << 20

var (
	// ErrNoToken is an answer plex.tv accepted but that carried no token.
	ErrNoToken = errors.New("claim: plex.tv answered without a server token")
	// ErrTooLarge is an answer over maxBody.
	ErrTooLarge = errors.New("claim: plex.tv's answer is too large")
)

// Exchanger trades claim codes with plex.tv.
type Exchanger struct {
	// BaseURL is plex.tv; empty means DefaultBaseURL.
	BaseURL string
	// HTTP is the client; nil means http.DefaultClient.
	HTTP *http.Client
}

// Exchange trades claim for the token of the server whose
// ProcessedMachineIdentifier is clientID: plex.tv files the token under the
// identity it is claimed as, which is the one every client sees.
func (e Exchanger) Exchange(ctx context.Context, claim, clientID string) (string, error) {
	if claim == "" || clientID == "" {
		return "", errors.New("claim: a claim code and the server's ProcessedMachineIdentifier are both required")
	}
	base := e.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	client := e.HTTP
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/api/claim/exchange?token="+url.QueryEscape(claim), nil)
	if err != nil {
		return "", errors.New("claim: build the exchange request")
	}
	// The headers Plex's own image sends: the server, as itself.
	for k, v := range map[string]string{
		"X-Plex-Client-Identifier": clientID,
		"X-Plex-Product":           "Plex Media Server",
		"X-Plex-Version":           "1.1",
		"X-Plex-Provides":          "server",
		"X-Plex-Platform":          "Linux",
		"X-Plex-Platform-Version":  "1.0",
		"X-Plex-Device-Name":       "PlexMediaServer",
		"X-Plex-Device":            "Linux",
	} {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		// A *url.Error quotes the URL, and the claim is in its query.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return "", fmt.Errorf("claim: reach %s: %w", base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return "", fmt.Errorf("claim: read plex.tv's answer: %w", err)
	}
	if len(body) > maxBody {
		return "", ErrTooLarge
	}
	// The body is not quoted: a refusal can echo the code back.
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("claim: plex.tv refused the exchange: %s", resp.Status)
	}
	token, err := tokenOf(body)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", ErrNoToken
	}
	return token, nil
}

// tokenOf reads <authentication-token>, which Plex's image takes, or else the
// root element's authToken or authenticationToken attribute.
func tokenOf(body []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	var attr string
	root := true
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return attr, nil
		}
		if err != nil {
			return "", errors.New("claim: plex.tv's answer is not XML")
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if se.Name.Local == "authentication-token" {
			var v string
			if err := dec.DecodeElement(&v, &se); err != nil {
				return "", errors.New("claim: plex.tv's answer is not XML")
			}
			return v, nil
		}
		if root {
			root = false
			for _, a := range se.Attr {
				if a.Name.Local == "authToken" || a.Name.Local == "authenticationToken" {
					attr = a.Value
				}
			}
		}
	}
}
