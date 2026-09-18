package mediaproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ResolvePath is the manager endpoint that authorizes a media request and
// answers with the file to serve.
const ResolvePath = "/internal/resolve"

// HTTPResolver asks the manager running beside Plex. Authorization has to
// happen there because only Plex can judge a token, and only that pod has the
// library database.
type HTTPResolver struct {
	// Endpoint returns the base URL of the manager to ask.
	Endpoint func(ctx context.Context) (string, error)
	Client   *http.Client
}

type resolveReply struct {
	File string `json:"file"`
}

// Resolve implements Resolver.
func (h *HTTPResolver) Resolve(ctx context.Context, partID, path string, header http.Header) (string, error) {
	base, err := h.Endpoint(ctx)
	if err != nil {
		return "", err
	}
	if base == "" {
		return "", ErrNoUpstream
	}

	q := url.Values{"part": {partID}, "path": {path}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+ResolvePath+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	// Carry the caller's Plex credentials so the manager can replay them.
	for name, values := range header {
		if strings.HasPrefix(strings.ToLower(name), "x-plex") {
			for _, v := range values {
				req.Header.Add(name, v)
			}
		}
	}

	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", ErrDenied
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("resolve: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var reply resolveReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return "", fmt.Errorf("resolve: %w", err)
	}
	return reply.File, nil
}
