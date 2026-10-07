package api_test

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

// fixture is one answer recorded from the owner's PMS 1.43.4
// (docs/livetv-pms-calls.md).
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/livetv/" + name)
	require.NoError(t, err)
	return string(b)
}

const (
	probeUUID   = "device://tv.plex.grabbers.hdhomerun/CAFE0001"
	probeLineup = "lineup://tv.plex.providers.epg.xmltv/http://10.96.55.197/livetv/probe/probe/xmltv.xml#probe"
)

func TestTheLiveTVCallsDecodeWhatPMSAnswered(t *testing.T) {
	c, calls := recorder(t, map[string]string{
		"GET /media/grabbers/devices":  fixture(t, "04-devices.json"),
		"POST /media/grabbers/devices": fixture(t, "03-add-device.json"),
		"GET /livetv/dvrs":             fixture(t, "06-dvrs.json"),
		"POST /livetv/dvrs":            fixture(t, "05-create-dvr.json"),
		"GET /livetv/epg/channelmap":   fixture(t, "07-channelmap.json"),
		"GET /identity":                `{"MediaContainer":{"size":0,"claimed":true,"machineIdentifier":"0123abcd","version":"1.43.4"}}`,
		"GET /activities": `{"MediaContainer":{"size":1,"Activity":[{"uuid":"a1","type":"provider.epg.load","cancellable":false,
			"userID":1,"title":"Refreshing EPG","subtitle":"Loading guide data","progress":42}]}}`,
	}, nil)
	ctx := t.Context()

	devs, err := c.Devices(ctx)
	require.NoError(t, err)
	assert.Equal(t, []plexapi.Device{{
		Key: "4", UUID: probeUUID, URI: "http://10.96.55.197", DeviceID: "CAFE0001", Model: "clustarr probe", State: "enabled",
	}}, devs)

	require.NoError(t, c.AddDevice(ctx, "http://169.254.47.1:47000"))

	dvrs, err := c.DVRs(ctx)
	require.NoError(t, err)
	require.Len(t, dvrs, 1)
	d := dvrs[0]
	assert.Equal(t, plexapi.Str("5"), d.Key)
	assert.Equal(t, "eng", d.Language)
	assert.Equal(t, probeLineup, d.Lineup)
	assert.Equal(t, "probe", d.LineupTitle)
	assert.Equal(t, "tv.plex.providers.epg.xmltv:5", d.EPGIdentifier)
	assert.Equal(t, []plexapi.DVRLineup{{ID: probeLineup, Title: "probe"}}, d.Lineups)
	require.Len(t, d.Devices, 1)
	assert.Equal(t, probeUUID, d.Devices[0].UUID)
	assert.Equal(t, 5, d.Devices[0].ParentID, "parentID is a number on the wire")

	created, err := c.CreateDVR(ctx, "eng", probeUUID, probeLineup)
	require.NoError(t, err)
	assert.Equal(t, plexapi.Str("5"), created.Key)

	m, err := c.EPGChannelMap(ctx, probeUUID, probeLineup)
	require.NoError(t, err)
	require.Len(t, m, 100)
	assert.Equal(t, plexapi.ChannelMapping{ChannelKey: "1000", DeviceIdentifier: "1000", LineupIdentifier: "1000", Favorite: "0"}, m[0])

	require.NoError(t, c.SaveChannelMap(ctx, "4", m[:2]))
	require.NoError(t, c.ReloadGuide(ctx, "5"))
	require.NoError(t, c.DeleteDVR(ctx, "5"))
	require.NoError(t, c.DeleteDevice(ctx, "4"))

	id, err := c.Identity(ctx)
	require.NoError(t, err)
	assert.Equal(t, "0123abcd", id)

	acts, err := c.Activities(ctx)
	require.NoError(t, err)
	assert.Equal(t, []plexapi.Activity{{UUID: "a1", Type: plexapi.ActivityGuideLoad, Title: "Refreshing EPG", Subtitle: "Loading guide data", Progress: 42}}, acts)

	q := func(raw string) url.Values {
		v, err := url.ParseQuery(raw)
		require.NoError(t, err)
		return v
	}
	byKey := map[string]call{}
	for _, cl := range *calls {
		byKey[cl.Method+" "+cl.Path] = cl
		assert.Equal(t, "tok", cl.Token, "%s %s carries the token", cl.Method, cl.Path)
	}
	assert.Equal(t, url.Values{"uri": {"http://169.254.47.1:47000"}}, q(byKey["POST /media/grabbers/devices"].Query))
	assert.Equal(t, url.Values{"language": {"eng"}, "device": {probeUUID}, "lineup": {probeLineup}}, q(byKey["POST /livetv/dvrs"].Query))
	assert.Equal(t, url.Values{"device": {probeUUID}, "lineup": {probeLineup}}, q(byKey["GET /livetv/epg/channelmap"].Query))
	assert.Equal(t, url.Values{
		"channelsEnabled":           {"1000,1001"},
		"channelMappingByKey[1000]": {"1000"}, "channelMapping[1000]": {"1000"},
		"channelMappingByKey[1001]": {"1001"}, "channelMapping[1001]": {"1001"},
	}, q(byKey["PUT /media/grabbers/devices/4/channelmap"].Query))
	for _, k := range []string{"POST /livetv/dvrs/5/reloadGuide", "DELETE /livetv/dvrs/5", "DELETE /media/grabbers/devices/4"} {
		assert.Contains(t, byKey, k)
	}
}

// mapOf is n mappings whose three identifiers are each the guide number, as
// clustarr's lineup and guide make them, numbered from 1000.
func mapOf(n int) []plexapi.ChannelMapping {
	out := make([]plexapi.ChannelMapping, n)
	for i := range out {
		id := fmt.Sprint(1000 + i)
		out[i] = plexapi.ChannelMapping{ChannelKey: id, DeviceIdentifier: id, LineupIdentifier: id}
	}
	return out
}

func TestMapBytesIsTheQueryPMSAcceptedAt454Channels(t *testing.T) {
	// Recorded: 454 four-digit channels were a 32,738-byte path and query
	// (the 37-byte path included), the largest PMS took.
	assert.Equal(t, 32701, plexapi.MapBytes(mapOf(454)))
	assert.Equal(t, 32701+37, len("/media/grabbers/devices/4/channelmap?")+plexapi.MapBytes(mapOf(454)))
}

func TestAChannelMapOverTheBudgetIsRefusedUnsent(t *testing.T) {
	c, calls := recorder(t, nil, nil)
	m := mapOf(500)
	require.Greater(t, plexapi.MapBytes(m), plexapi.MaxChannelMapBytes)
	err := c.SaveChannelMap(t.Context(), "4", m)
	require.ErrorIs(t, err, plexapi.ErrChannelMapTooLarge)
	assert.Empty(t, *calls, "nothing reaches PMS")

	fits := mapOf(400)
	require.LessOrEqual(t, plexapi.MapBytes(fits), plexapi.MaxChannelMapBytes)
	require.NoError(t, c.SaveChannelMap(t.Context(), "4", fits))
	assert.Len(t, *calls, 1)
}

func TestAnEmptyChannelMapIsRefusedUnsent(t *testing.T) {
	// Recorded: a PUT without the map cleared the device's whole map.
	c, calls := recorder(t, nil, nil)
	require.ErrorIs(t, c.SaveChannelMap(t.Context(), "4", nil), plexapi.ErrEmptyChannelMap)
	assert.Empty(t, *calls)
}

func TestIdentityWithoutAMachineIdentifierIsAnError(t *testing.T) {
	c, _ := recorder(t, map[string]string{"GET /identity": `{"MediaContainer":{"size":0}}`}, nil)
	_, err := c.Identity(t.Context())
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "machineIdentifier"))
}
