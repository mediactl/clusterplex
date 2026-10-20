package api_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

// A Live TV grab as PMS 1.43.4 listed it on talos (2026-10-20), beside a
// subscription refresh: the grab's Context names its live session.
const grabActivities = `{"MediaContainer":{"size":2,"Activity":[
	{"uuid":"1f6b7fe0","type":"provider.subscription.refresh","cancellable":false,"userID":1,"title":"Refreshing Sub","subtitle":"Grabbing","progress":0},
	{"uuid":"1c4e9a90","type":"grabber.grab","cancellable":true,"userID":1,"title":"Recording","subtitle":"Live TV - Session","progress":2,
	 "Context":{"deviceID":"1","grabber":"tv.plex.grabbers.hdhomerun","itemKey":"","itemMediaIndex":"0","itemRatingKey":"","source":"provider://tv.plex.providers.epg.xmltv","status":"inprogress","subscriptionID":"90","transcode_session":"72ca8128-32e2-4500-804a-7c7df1285b7a"}}]}}`

func TestAGrabsActivityNamesItsLiveSession(t *testing.T) {
	c, _ := recorder(t, map[string]string{"GET /activities": grabActivities}, nil)
	acts, err := c.Activities(t.Context())
	require.NoError(t, err)
	require.Len(t, acts, 2)
	assert.Equal(t, plexapi.ActivityGrab, acts[1].Type)
	assert.Equal(t, plexapi.Str("72ca8128-32e2-4500-804a-7c7df1285b7a"), acts[1].Context.TranscodeSession)
	assert.Empty(t, acts[0].Context.TranscodeSession)
}

func TestAnActivityIsCancelledByItsUUID(t *testing.T) {
	c, calls := recorder(t, map[string]string{}, nil)
	require.NoError(t, c.CancelActivity(t.Context(), "1c4e9a90"))
	require.Len(t, *calls, 1)
	assert.Equal(t, "DELETE", (*calls)[0].Method)
	assert.Equal(t, "/activities/1c4e9a90", (*calls)[0].Path)
}

func TestLiveSessionsAndWhatIsPlayingAreReadByKeyAndRatingKey(t *testing.T) {
	c, _ := recorder(t, map[string]string{
		"GET /livetv/sessions": `{"MediaContainer":{"size":1,"Metadata":[{"key":"/livetv/sessions/72ca8128-32e2-4500-804a-7c7df1285b7a",
			"ratingKey":"16880","guid":"tv.plex.xmltv://movie/The%20Drew%20Barrymore%20Show","live":"1","title":"The Drew Barrymore Show"}]}}`,
		"GET /status/sessions": `{"MediaContainer":{"size":2,"Metadata":[{"key":"/library/metadata/14082","ratingKey":14082,"type":"movie"},
			{"key":"/livetv/sessions/72ca8128-32e2-4500-804a-7c7df1285b7a","ratingKey":"16880","live":1}]}}`,
	}, nil)
	ls, err := c.LiveSessions(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []plexapi.LiveSession{{Key: "/livetv/sessions/72ca8128-32e2-4500-804a-7c7df1285b7a", RatingKey: "16880", Title: "The Drew Barrymore Show"}}, ls)

	playing, err := c.Playing(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []plexapi.PlayingItem{
		{Key: "/library/metadata/14082", RatingKey: "14082"},
		{Key: "/livetv/sessions/72ca8128-32e2-4500-804a-7c7df1285b7a", RatingKey: "16880"},
	}, playing, "a ratingKey is a number or a string on the wire")
}
