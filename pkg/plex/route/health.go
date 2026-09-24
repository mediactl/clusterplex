package plexroute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrLibrary marks an Answering failure that came from the library check
// rather than from /identity: Plex is up and answering, but not for anything
// that needs its database. Callers that watch a Plex which has not finished
// starting need the distinction, because a library that is still migrating
// looks exactly like one that has hung.
var ErrLibrary = errors.New("plex library not answering")

// healthPath is the cheapest endpoint that proves Plex has finished starting.
// It needs no authentication on an unclaimed server and no database work.
const healthPath = "/identity"

// answeringPath is the cheapest request that makes Plex read its library.
//
// /identity is served from memory. A Plex whose every database session is
// stuck — which is what a deadlock in its database layer looks like from
// outside — goes on answering it in milliseconds while every real request
// hangs, so a watch built on /identity alone kept such a pod in service for
// minutes. Listing the library sections is one small query, and it hangs
// exactly when the rest of Plex does.
const answeringPath = "/library/sections"

// healthTimeout bounds a single check. Plex answers these in milliseconds when
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
	return get(ctx, baseURL, healthPath, "")
}

// Answering reports whether Plex is serving and can still reach its library.
//
// The library check needs Plex's local admin token: a claimed server refuses
// an unauthenticated caller before it touches the database, which would prove
// nothing either way. With no token to offer this is the same check as
// Serving, which is also what a server that has not been claimed yet gets.
func Answering(ctx context.Context, baseURL, token string) error {
	if err := Serving(ctx, baseURL); err != nil {
		return err
	}
	if token == "" {
		return nil
	}
	if err := get(ctx, baseURL, answeringPath, token); err != nil {
		return fmt.Errorf("%w: %w", ErrLibrary, err)
	}
	return nil
}

// get makes one bounded request and reports whether Plex answered it.
//
// Any status below 500 is an answer, a refusal included: a claimed server
// declining a caller is a working server. From 500 up is the server saying it
// cannot answer, and not answering at all is the case that matters most.
func get(ctx context.Context, baseURL, path, token string) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	url := strings.TrimSuffix(baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
		req.Header.Set("Accept", "application/xml")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("plex at %s is not serving: %s %s", baseURL, path, resp.Status)
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
