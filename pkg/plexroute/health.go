package plexroute

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// healthPath is the cheapest endpoint that proves Plex has finished starting.
// It needs no authentication on an unclaimed server and no database work.
const healthPath = "/identity"

// healthTimeout bounds a single check. Plex answers this in milliseconds when
// it is up, so a slow answer is itself a symptom.
const healthTimeout = 5 * time.Second

// Serving reports whether Plex is answering requests, as opposed to merely
// holding its port open.
//
// The distinction is the whole point. Plex binds 32400 within a second of
// starting and answers 503 to everything until it has finished initialising —
// and it can abort part way through and sit in that state indefinitely, port
// open, serving nothing. Dialling the port cannot tell the two apart, so a pod
// in that state would go on advertising itself as able to serve.
func Serving(ctx context.Context, baseURL string) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	url := strings.TrimSuffix(baseURL, "/") + healthPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	// A claimed server refuses an unauthenticated caller, which is a working
	// server declining us rather than a broken one. Anything from 500 up is
	// the server saying it cannot answer.
	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("plex at %s is not serving: %s", baseURL, resp.Status)
	}
	return nil
}

// WaitServing blocks until Plex answers or the timeout passes, reporting the
// last reason it did not.
func WaitServing(ctx context.Context, baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := Serving(ctx, baseURL)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return err
		}
		sleep(ctx, dialInterval)
	}
}
