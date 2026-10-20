package livetv

import (
	"context"
	"log/slog"
	"strings"
	"time"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

// DefaultStaleGrabAfter is how long a Live TV grab may go unwatched before
// StaleGrabs ends it: past PMS's own 300 s wait for a tuner, and long
// enough for a player that is starting, buffering or between its
// timelines.
const DefaultStaleGrabAfter = 5 * time.Minute

// liveGrabSubtitle begins a Live TV session's grab ("Live TV - Session
// <id>"); a DVR recording's is the programme's title.
const liveGrabSubtitle = "Live TV - Session"

// GrabPMS is what StaleGrabs reads and cancels.
type GrabPMS interface {
	Activities(ctx context.Context) ([]plexapi.Activity, error)
	LiveSessions(ctx context.Context) ([]plexapi.LiveSession, error)
	Playing(ctx context.Context) ([]plexapi.PlayingItem, error)
	CancelActivity(ctx context.Context, uuid string) error
}

// StaleGrabs ends the Live TV grabs no player watches (owner, 2026-10-20).
//
// PMS stops a live grab only when its client reports the playback stopped
// ("Recorder: No more consumers"): stopping the client's transcode does
// not, and a client killed or cut off mid-play reports nothing. Its grab
// then holds one of the device's tuners until PMS restarts. On talos one
// such grab, from about 15:00, made every tune for six hours wait PMS's
// 300 s for a tuner and fail as "The recording conflicts with other
// recordings"; each failed tune stayed listed and held the tuner in turn.
//
// A grab is ended by cancelling its activity, the dashboard's cancel and
// the one call that ends it (CancelActivity), once no player has watched
// its live session for Grace. A DVR recording, which runs with no player
// by design, is never touched. Not safe for concurrent use.
type StaleGrabs struct {
	PMS GrabPMS
	// Grace is how long a grab may go unwatched; zero is
	// DefaultStaleGrabAfter.
	Grace time.Duration
	// Now is the clock; nil is time.Now.
	Now    func() time.Time
	Logger *slog.Logger

	// unwatched is when each grab (by activity UUID) was first seen
	// unwatched in its current run of passes.
	unwatched map[string]time.Time
}

// Pass reads PMS once and cancels each Live TV grab unwatched for Grace,
// returning their live sessions' keys. An error is a read that failed:
// nothing is cancelled on a partial view. A cancel that fails is tried
// again by the next pass.
func (w *StaleGrabs) Pass(ctx context.Context) ([]string, error) {
	acts, err := w.PMS.Activities(ctx)
	if err != nil {
		return nil, err
	}
	live, err := w.PMS.LiveSessions(ctx)
	if err != nil {
		return nil, err
	}
	playing, err := w.PMS.Playing(ctx)
	if err != nil {
		return nil, err
	}
	watchedKeys, watchedItems := map[string]bool{}, map[string]bool{}
	for _, p := range playing {
		watchedKeys[p.Key] = true
		if p.RatingKey != "" {
			watchedItems[string(p.RatingKey)] = true
		}
	}
	itemOf := map[string]string{}
	for _, l := range live {
		itemOf[l.Key] = string(l.RatingKey)
	}
	if w.unwatched == nil {
		w.unwatched = map[string]time.Time{}
	}
	now := w.now()
	grace := w.Grace
	if grace <= 0 {
		grace = DefaultStaleGrabAfter
	}
	seen := map[string]bool{}
	var ended []string
	for _, a := range acts {
		session := string(a.Context.TranscodeSession)
		if a.Type != plexapi.ActivityGrab || !strings.HasPrefix(a.Subtitle, liveGrabSubtitle) || session == "" {
			continue
		}
		seen[a.UUID] = true
		key := "/livetv/sessions/" + session
		if watchedKeys[key] || (itemOf[key] != "" && watchedItems[itemOf[key]]) {
			delete(w.unwatched, a.UUID)
			continue
		}
		since, ok := w.unwatched[a.UUID]
		if !ok {
			w.unwatched[a.UUID] = now
			continue
		}
		if now.Sub(since) < grace {
			continue
		}
		if err := w.PMS.CancelActivity(ctx, a.UUID); err != nil {
			w.logger().Warn("Live TV: could not end a grab no player watches", "session", key, "error", err)
			continue
		}
		delete(w.unwatched, a.UUID)
		w.logger().Info("Live TV: ended a grab no player watched; it held a tuner",
			"session", key, "unwatched", now.Sub(since).Round(time.Second))
		ended = append(ended, key)
	}
	for uuid := range w.unwatched {
		if !seen[uuid] {
			delete(w.unwatched, uuid)
		}
	}
	return ended, nil
}

func (w *StaleGrabs) now() time.Time {
	if w.Now == nil {
		return time.Now()
	}
	return w.Now()
}

func (w *StaleGrabs) logger() *slog.Logger {
	if w.Logger == nil {
		return slog.Default()
	}
	return w.Logger
}
