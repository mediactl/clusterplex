package relay

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mediactl/clusterplex/pkg/video"
)

// Plex's transcoder logs both turns of its throttle to PMS (finding P).
const (
	slothMessage = "Throttle - Going into sloth mode."
	workMessage  = "Throttle - Getting back to work."
	// slothRemaining is the remaining Plex's transcoder reports in sloth
	// mode: none known.
	slothRemaining = "2147483647"
)

// canThrottle reports whether Plex's answer to a progress PUT throttles the
// job: <MediaContainer size="0" canThrottle="1">, which PMS answers while
// the job is its throttle buffer (TranscoderThrottleBuffer, 60 s) ahead of
// what the client has read.
func canThrottle(answer []byte) bool {
	var mc struct {
		CanThrottle string `xml:"canThrottle,attr"`
	}
	return xml.Unmarshal(answer, &mc) == nil && mc.CanThrottle == "1"
}

// sloth holds a throttled conversion as Plex's transcoder holds itself: it
// logs sloth mode, PUTs progress every ThrottlePoll without a speed and
// with no remaining time known, and returns at the first answer that does
// not throttle, logging getting back to work. A failed PUT ends the hold
// too: Plex's transcoder carries on past one, and a PMS that has stopped
// answering must not wedge the job.
func (r *Relay) sloth(ctx context.Context, job video.Job, query string) error {
	r.logToPlex(ctx, job, slothMessage)
	q := slothQuery(query)
	t := time.NewTicker(r.throttlePoll())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		answer, err := r.PMS.Ask(ctx, http.MethodPut, job.ProgressURL, "", q, http.NoBody, job.Token)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || !canThrottle(answer) {
			r.logToPlex(ctx, job, workMessage)
			return nil
		}
	}
}

func (r *Relay) throttlePoll() time.Duration {
	if r.ThrottlePoll <= 0 {
		return DefaultThrottlePoll
	}
	return r.ThrottlePoll
}

// logToPlex posts one of the transcoder's log lines, as Plex's does:
// <progress URL>/log?level=3&message=<message, %20 for spaces>.
func (r *Relay) logToPlex(ctx context.Context, job video.Job, msg string) {
	_ = r.PMS.Send(ctx, http.MethodPost, job.ProgressURL, "log", "level=3&message="+url.PathEscape(msg), http.NoBody, job.Token)
}

// slothQuery is a progress query as sloth mode sends it: without speed,
// remaining slothRemaining, every other key in its place.
func slothQuery(query string) string {
	var out []string
	for _, kv := range strings.Split(query, "&") {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "speed":
		case "remaining":
			out = append(out, "remaining="+slothRemaining)
		default:
			out = append(out, kv)
		}
	}
	return strings.Join(out, "&")
}
