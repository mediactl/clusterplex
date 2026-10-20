package livetv_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	plexlivetv "github.com/mediactl/clusterplex/pkg/plex/livetv"
)

// grabPMS is PMS's activities, live sessions and players.
type grabPMS struct {
	activities []plexapi.Activity
	live       []plexapi.LiveSession
	playing    []plexapi.PlayingItem
	cancelled  []string
	cancelErr  error
	readErr    error
}

func (p *grabPMS) Activities(context.Context) ([]plexapi.Activity, error) {
	return p.activities, p.readErr
}

func (p *grabPMS) LiveSessions(context.Context) ([]plexapi.LiveSession, error) { return p.live, nil }

func (p *grabPMS) Playing(context.Context) ([]plexapi.PlayingItem, error) { return p.playing, nil }

func (p *grabPMS) CancelActivity(_ context.Context, uuid string) error {
	if p.cancelErr != nil {
		return p.cancelErr
	}
	p.cancelled = append(p.cancelled, uuid)
	return nil
}

func liveGrab(uuid, session string) plexapi.Activity {
	return plexapi.Activity{
		UUID: uuid, Type: plexapi.ActivityGrab, Title: "Recording", Subtitle: "Live TV - Session " + session,
		Context: plexapi.ActivityContext{TranscodeSession: plexapi.Str(session)},
	}
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time      { return c.now }
func (c *clock) add(d time.Duration) { c.now = c.now.Add(d) }
func watchdog(pms *grabPMS, c *clock) *plexlivetv.StaleGrabs {
	return &plexlivetv.StaleGrabs{PMS: pms, Grace: 5 * time.Minute, Now: c.Now}
}

func TestALiveTVGrabNoPlayerWatchesIsCancelledAfterTheGrace(t *testing.T) {
	// A client killed mid-play never reports it stopped, and its grab
	// holds a tuner until PMS restarts (talos, 2026-10-20).
	pms := &grabPMS{activities: []plexapi.Activity{liveGrab("g1", "s1")}, live: []plexapi.LiveSession{{Key: "/livetv/sessions/s1", RatingKey: "16880"}}}
	c := &clock{now: time.Unix(1792530000, 0)}
	w := watchdog(pms, c)

	ended, err := w.Pass(t.Context())
	require.NoError(t, err)
	assert.Empty(t, ended, "first seen unwatched: not yet")
	c.add(4 * time.Minute)
	ended, _ = w.Pass(t.Context())
	assert.Empty(t, ended)
	c.add(time.Minute)
	ended, err = w.Pass(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"/livetv/sessions/s1"}, ended)
	assert.Equal(t, []string{"g1"}, pms.cancelled)
}

func TestAWatchedLiveTVGrabIsLeftAlone(t *testing.T) {
	for name, playing := range map[string]plexapi.PlayingItem{
		"by its live session's key": {Key: "/livetv/sessions/s1"},
		"by its item's ratingKey":   {Key: "/library/metadata/16880", RatingKey: "16880"},
	} {
		t.Run(name, func(t *testing.T) {
			pms := &grabPMS{
				activities: []plexapi.Activity{liveGrab("g1", "s1")}, live: []plexapi.LiveSession{{Key: "/livetv/sessions/s1", RatingKey: "16880"}},
				playing: []plexapi.PlayingItem{playing},
			}
			c := &clock{now: time.Unix(1792530000, 0)}
			w := watchdog(pms, c)
			for range 4 {
				_, err := w.Pass(t.Context())
				require.NoError(t, err)
				c.add(5 * time.Minute)
			}
			assert.Empty(t, pms.cancelled)
		})
	}
}

func TestTheGraceStartsOverWhenAPlayerReturns(t *testing.T) {
	pms := &grabPMS{activities: []plexapi.Activity{liveGrab("g1", "s1")}, live: []plexapi.LiveSession{{Key: "/livetv/sessions/s1"}}}
	c := &clock{now: time.Unix(1792530000, 0)}
	w := watchdog(pms, c)
	_, _ = w.Pass(t.Context())
	c.add(4 * time.Minute)
	pms.playing = []plexapi.PlayingItem{{Key: "/livetv/sessions/s1"}}
	_, _ = w.Pass(t.Context())
	pms.playing = nil
	c.add(time.Minute)
	_, _ = w.Pass(t.Context()) // first seen unwatched again: the grace starts here
	c.add(4 * time.Minute)
	_, _ = w.Pass(t.Context())
	assert.Empty(t, pms.cancelled, "unwatched for 4 minutes since a player last watched it")
	c.add(time.Minute)
	_, _ = w.Pass(t.Context())
	assert.Equal(t, []string{"g1"}, pms.cancelled)
}

func TestOnlyLiveTVGrabsAreEverCancelled(t *testing.T) {
	// A DVR recording runs with no player by design.
	recording := plexapi.Activity{
		UUID: "r1", Type: plexapi.ActivityGrab, Title: "Recording", Subtitle: "The Drew Barrymore Show",
		Context: plexapi.ActivityContext{TranscodeSession: "s9"},
	}
	other := plexapi.Activity{UUID: "a1", Type: plexapi.ActivityGuideLoad, Subtitle: "Live TV - Session"}
	nameless := liveGrab("g2", "")
	pms := &grabPMS{activities: []plexapi.Activity{recording, other, nameless}}
	c := &clock{now: time.Unix(1792530000, 0)}
	w := watchdog(pms, c)
	for range 3 {
		_, err := w.Pass(t.Context())
		require.NoError(t, err)
		c.add(10 * time.Minute)
	}
	assert.Empty(t, pms.cancelled)
}

func TestACancelThatFailsIsTriedAgainNextPass(t *testing.T) {
	pms := &grabPMS{activities: []plexapi.Activity{liveGrab("g1", "s1")}, cancelErr: errors.New("500")}
	c := &clock{now: time.Unix(1792530000, 0)}
	w := watchdog(pms, c)
	_, _ = w.Pass(t.Context())
	c.add(6 * time.Minute)
	ended, err := w.Pass(t.Context())
	require.NoError(t, err, "one grab's failure is not the pass's")
	assert.Empty(t, ended)
	pms.cancelErr = nil
	ended, _ = w.Pass(t.Context())
	assert.Equal(t, []string{"/livetv/sessions/s1"}, ended)
}

func TestAGrabThatEndsOnItsOwnIsForgotten(t *testing.T) {
	pms := &grabPMS{activities: []plexapi.Activity{liveGrab("g1", "s1")}}
	c := &clock{now: time.Unix(1792530000, 0)}
	w := watchdog(pms, c)
	_, _ = w.Pass(t.Context())
	pms.activities = nil
	c.add(3 * time.Minute)
	_, _ = w.Pass(t.Context())
	pms.activities = []plexapi.Activity{liveGrab("g1", "s1")}
	c.add(3 * time.Minute)
	_, _ = w.Pass(t.Context())
	assert.Empty(t, pms.cancelled, "seen again, it starts its grace again")
}

func TestAPassThatCannotReadPMSCancelsNothing(t *testing.T) {
	pms := &grabPMS{activities: []plexapi.Activity{liveGrab("g1", "s1")}}
	c := &clock{now: time.Unix(1792530000, 0)}
	w := watchdog(pms, c)
	_, _ = w.Pass(t.Context())
	c.add(10 * time.Minute)
	pms.readErr = errors.New("connection refused")
	_, err := w.Pass(t.Context())
	require.Error(t, err)
	assert.Empty(t, pms.cancelled)
}
