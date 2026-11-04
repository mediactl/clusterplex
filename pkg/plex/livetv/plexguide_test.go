// Package livetv adds each clustarr IPTVProvider to Plex as DVRs (clustarr's
// Live TV spec 2026-10-07 §6.1, amended by its direct-tuners design
// 2026-10-27). clustarr splits a provider's channels into blocks of
// contiguous numbers, because PMS takes at most ~454 channels in one
// device's channel map; each block is one HDHomeRun device at its own path
// on clustarr's livetv Service (status.blocks[].uri,
// http://<service>/hdhr/<deviceID>), which PMS reaches from its pod as it
// reaches every cluster Service, and one DVR over that block's slice of the
// guide. Nothing in a Plex pod proxies or listens for it. The lease holder
// registers each block it holds the Lease for, creates its DVR, saves its
// channel map and reloads its guide when they change. It manages only
// devices carrying a live block's device id or under a livetv Service's
// /hdhr/ (rule M): a block's device at any other URI is clustarr's at a
// stale address and is replaced. It never deletes, saves, reloads or creates
// a DVR while PMS loads a guide: a DVR deleted during provider.epg.load
// deadlocked PMS (recorded). Creating a DVR, saving a map and reloading a

package livetv_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	"github.com/mediactl/clusterplex/pkg/plex/livetv"
)

const (
	cloudLineup = "lineup://tv.plex.providers.epg.cloud/657c8183afd0f02cd353633f#DIRECTV%20Mobile%20%281009%20channels%29"
	cloudKey    = "657c8183afd0f02cd353633f-5fc705e2088e7d002d4c6de2"
)

// tunerMaps answers <uri>/plexmap.json from a map of URI to body, as the
// tuner serves a Plex block's channel map.
type tunerMaps map[string]string

func (m tunerMaps) RoundTrip(r *http.Request) (*http.Response, error) {
	u := *r.URL
	u.RawQuery = ""
	body, ok := m[strings.TrimSuffix(u.String(), "/plexmap.json")]
	if !ok || !strings.HasSuffix(u.Path, "/plexmap.json") {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// plexBlock is block 0 on the Plex cloud lineup, its tuner serving a map
// of channel 1070 on A&E HD (265).
func plexBlock(r *rig) *http.Client {
	return &http.Client{Transport: tunerMaps{
		r.block(0).URI: `[{"deviceIdentifier":"1070","channelKey":"` + cloudKey + `","lineupIdentifier":"265"}]`,
	}}
}

func TestAPlexBlockGetsACloudDVRAndTheTunersMap(t *testing.T) {
	r := newRig(t)
	r.conv.HTTP = plexBlock(r)
	b := r.block(0)
	b.Lineup, b.GuidePath, b.GuideHash = cloudLineup, "", ""
	r.converge(t, 6, provider(b))

	devices, dvrs := r.pms.state()
	require.Len(t, devices, 1)
	require.Len(t, dvrs, 1)
	assert.Equal(t, cloudLineup, dvrs[0].Lineup, "the DVR's guide is the Plex lineup")
	q := r.pms.saved[string(devices[0].Key)]
	require.NotNil(t, q, "the map was saved")
	assert.Equal(t, cloudKey, q.Get("channelMappingByKey[1070]"))
	assert.Equal(t, "265", q.Get("channelMapping[1070]"))
	assert.Equal(t, "1070", q.Get("channelsEnabled"))
	assert.Zero(t, r.pms.epgMapReads, "PMS's own pairing is never asked for a Plex block")
}

func TestAPlexBlockIsNeverReloaded(t *testing.T) {
	r := newRig(t)
	r.conv.HTTP = plexBlock(r)
	r.conv.GuideEmpty = func(context.Context, string) (bool, error) { return true, nil }
	b := r.block(0)
	b.Lineup, b.GuidePath, b.GuideHash = cloudLineup, "", ""
	r.converge(t, 6, provider(b))
	r.pms.takeWrites()

	b.GuideHash = "g-moved" // nothing of clustarr's guide applies to a Plex block
	r.converge(t, 4, provider(b))
	assert.Zero(t, count(r.pms.takeWrites(), "POST /livetv/dvrs/"), "no reloadGuide, by guideHash or as an empty guide")
}

func TestAPlexBlocksMapIsSavedAgainWhenItsLineupHashMoves(t *testing.T) {
	r := newRig(t)
	r.conv.HTTP = plexBlock(r)
	b := r.block(0)
	b.Lineup, b.GuidePath, b.GuideHash = cloudLineup, "", ""
	r.converge(t, 6, provider(b))
	devices, _ := r.pms.state()
	before := r.pms.saves[string(devices[0].Key)]
	b.LineupHash = "l-moved"
	r.converge(t, 4, provider(b))
	assert.Equal(t, before+1, r.pms.saves[string(devices[0].Key)])
}

// PMS replaces a Plex block's channel map with a pairing of its own at
// the end of a load that failed (2026-11-04): the map is read back once
// the DVR's refreshedAt moves, saved again when it differs, and not again
// within the hour however often PMS replaces it.
func TestAChannelMapPMSReplacedIsSavedAgainOnceAnHour(t *testing.T) {
	r := newRig(t)
	r.conv.HTTP = plexBlock(r)
	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	r.conv.Now = func() time.Time { return now }
	b := r.block(0)
	b.Lineup, b.GuidePath, b.GuideHash = cloudLineup, "", ""
	r.converge(t, 6, provider(b))
	devices, _ := r.pms.state()
	key := string(devices[0].Key)
	r.pms.takeWrites()
	reads := r.pms.deviceReads

	// The load ends with the map as saved: nothing to do.
	r.pms.replaceMap(key, nil, now.Unix())
	delete(r.pms.mapOverride, key)
	r.converge(t, 2, provider(b))
	assert.Greater(t, r.pms.deviceReads, reads, "read back once the DVR refreshed")
	assert.Zero(t, count(r.pms.takeWrites(), "PUT "))
	reads = r.pms.deviceReads
	r.converge(t, 2, provider(b))
	assert.Equal(t, reads, r.pms.deviceReads, "not read again until the DVR refreshes or ten minutes pass")

	// PMS pairs 1070 with another lineup channel and adds one of its own.
	r.pms.replaceMap(key, []plexapi.ChannelMapping{
		{ChannelKey: cloudKey, DeviceIdentifier: "1070", LineupIdentifier: "355", Enabled: "1"},
		{ChannelKey: cloudKey, DeviceIdentifier: "1071", LineupIdentifier: "356", Enabled: "1"},
	}, now.Add(time.Minute).Unix())
	r.converge(t, 4, provider(b))
	writes := r.pms.takeWrites()
	assert.Equal(t, 1, count(writes, "PUT "), "%v", writes)
	assert.Equal(t, "265", r.pms.saved[key].Get("channelMapping[1070]"), "the tuner's pairing again")
	assert.Contains(t, r.ev.reasons(), livetv.ReasonChannelMapRestored)

	// PMS replaces it again at that load's end: told once, not saved again
	// within the hour.
	r.pms.replaceMap(key, []plexapi.ChannelMapping{
		{ChannelKey: cloudKey, DeviceIdentifier: "1070", LineupIdentifier: "355", Enabled: "1"},
	}, now.Add(2*time.Minute).Unix())
	before := len(r.ev.reasons())
	r.converge(t, 4, provider(b))
	r.converge(t, 4, provider(b))
	assert.Zero(t, count(r.pms.takeWrites(), "PUT "))
	assert.Equal(t, before+1, len(r.ev.reasons()), "recorded once")

	now = now.Add(2 * time.Hour)
	r.converge(t, 4, provider(b))
	assert.Equal(t, 1, count(r.pms.takeWrites(), "PUT "), "saved again after the hour")
}
