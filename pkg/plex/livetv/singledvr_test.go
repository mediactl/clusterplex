package livetv_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	"github.com/mediactl/clusterplex/pkg/plex/livetv"
)

// single is p with one DVR over every block's device, on the whole lineup's
// guide (clustarr's status.dvr; its single-DVR design, 2026-11-16).
func single(p livetv.Provider) livetv.Provider {
	p.DVR = &livetv.DVR{Title: "polutv", GuideURI: "http://" + address + "/livetv/media/polutv/xmltv.xml", GuideHash: "whole-1"}
	return p
}

const singleLineup = "lineup://tv.plex.providers.epg.xmltv/http://" + address + "/livetv/media/polutv/xmltv.xml#polutv"

func deviceIDs(d plexapi.DVR) []string {
	var out []string
	for _, dev := range d.Devices {
		out = append(out, dev.DeviceID)
	}
	return out
}

func TestOneDVRHoldsEveryBlocksDevice(t *testing.T) {
	r := newRig(t)
	p := single(provider(r.block(0), r.block(1), r.block(2)))

	res := r.converge(t, 16, p)
	writes := r.pms.takeWrites()
	assert.True(t, res.DVRChanged)

	devs, dvrs := r.pms.state()
	require.Len(t, devs, 3)
	require.Len(t, dvrs, 1, "one DVR, not one a block")
	assert.Equal(t, singleLineup, dvrs[0].Lineup)
	assert.ElementsMatch(t, []string{"B0000000", "B0000001", "B0000002"}, deviceIDs(dvrs[0]))
	assert.Equal(t, 1, count(writes, "POST /livetv/dvrs")-count(writes, "POST /livetv/dvrs/"), "one create")
	assert.Equal(t, 2, count(writes, "PUT /livetv/dvrs/"), "the other two devices are attached")
	for _, d := range devs {
		assert.Equal(t, 1, r.pms.saves[string(d.Key)], "each device keeps a map of its own: %s", d.DeviceID)
	}
	assert.Equal(t, 0, count(writes, "POST /livetv/dvrs/"), "no reload: the saves loaded the guide")

	t.Run("a second pass writes nothing", func(t *testing.T) {
		res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
		require.NoError(t, err)
		assert.Equal(t, livetv.Converged, res.Status)
		assert.Empty(t, r.pms.takeWrites())
	})

	t.Run("the whole guide's new hash reloads the one guide once", func(t *testing.T) {
		p.DVR.GuideHash = "whole-2"
		r.converge(t, 4, p)
		assert.Equal(t, []string{"POST /livetv/dvrs/" + string(dvrs[0].Key) + "/reloadGuide"}, r.pms.takeWrites())
	})

	t.Run("a block's guide hash is not the DVR's", func(t *testing.T) {
		p.Blocks[1].GuideHash = "moved"
		r.converge(t, 2, p)
		assert.Empty(t, r.pms.takeWrites())
	})

	t.Run("a new block's device is attached", func(t *testing.T) {
		p.Blocks = append(p.Blocks, r.block(3))
		r.converge(t, 6, p)
		_, dvrs := r.pms.state()
		require.Len(t, dvrs, 1)
		assert.Len(t, dvrs[0].Devices, 4)
	})
}

// The cutover: a provider with a DVR a block moves onto one DVR. Each block
// DVR is deleted, between guide loads, and its device joins the one DVR.
func TestAProviderMovesFromADVRABlockToOneDVR(t *testing.T) {
	r := newRig(t)
	p := provider(r.block(0), r.block(1), r.block(2))
	r.converge(t, 8, p)
	_, before := r.pms.state()
	require.Len(t, before, 3)
	r.pms.takeWrites()

	p = single(p)
	res := r.converge(t, 24, p)
	assert.True(t, res.DVRChanged)
	devs, dvrs := r.pms.state()
	require.Len(t, devs, 3, "the devices are the same")
	require.Len(t, dvrs, 1)
	assert.Equal(t, singleLineup, dvrs[0].Lineup)
	assert.Len(t, dvrs[0].Devices, 3)
	writes := r.pms.takeWrites()
	assert.Equal(t, 3, count(writes, "DELETE /livetv/dvrs/"))
	assert.Equal(t, 0, count(writes, "DELETE /media/grabbers/devices/"), "no device is removed")
	assert.Equal(t, 0, count(writes, "POST /media/grabbers/devices"), "nor registered again")
	for _, d := range devs {
		assert.Equal(t, 2, r.pms.saves[string(d.Key)], "the map is saved again on the new DVR: %s", d.DeviceID)
	}
}

// The way back: status.dvr gone, the one DVR is deleted and each block gets
// its own again.
func TestAProviderMovesBackToADVRABlock(t *testing.T) {
	r := newRig(t)
	p := provider(r.block(0), r.block(1), r.block(2))
	r.converge(t, 16, single(p))
	_, dvrs := r.pms.state()
	require.Len(t, dvrs, 1)
	r.pms.takeWrites()

	r.converge(t, 24, p)
	devs, dvrs := r.pms.state()
	require.Len(t, devs, 3)
	require.Len(t, dvrs, 3)
	for i, d := range dvrs {
		assert.Equal(t, []string{fmt.Sprintf("B000000%d", i)}, deviceIDs(d))
		assert.Contains(t, d.Lineup, "/hdhr/")
	}
	assert.Equal(t, 1, count(r.pms.takeWrites(), "DELETE /livetv/dvrs/"))
}

// A create that fails after the block DVRs are gone is tried again: the
// pass reports it, and a later one makes the DVR.
func TestAFailedCreateOfTheOneDVRIsTriedAgain(t *testing.T) {
	r := newRig(t)
	p := provider(r.block(0), r.block(1))
	r.converge(t, 8, p)
	p = single(p)
	r.pms.failOnce(http.MethodPost, "/livetv/dvrs", http.StatusInternalServerError)

	failed := false
	for range 24 {
		res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
		r.pms.endLoad()
		if err != nil {
			failed = true
			assert.Equal(t, livetv.Pending, res.Status)
			continue
		}
		if res.Status == livetv.Converged {
			break
		}
	}
	require.True(t, failed, "the create failed once")
	_, dvrs := r.pms.state()
	require.Len(t, dvrs, 1)
	assert.Len(t, dvrs[0].Devices, 2)
}

// Under the managers' guide builder the one DVR's guide is asked for once a
// pass with the whole guide's URL and hash, PMS never reloads it, and the
// loads its create and saves start are cancelled rather than waited out: a
// whole lineup's load is half an hour, and every write waits for it.
func TestTheOneDVRsGuideIsTheBuilders(t *testing.T) {
	r := newRig(t)
	built := &fakeXMLTVGuides{}
	r.conv.XMLTVGuides = built
	r.conv.GuideEmpty = nil
	p := single(provider(r.block(0), r.block(1), r.block(2)))

	for i := 0; ; i++ {
		require.Less(t, i, 40, "no convergence")
		res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
		require.NoError(t, err)
		if res.Status == livetv.Converged { // no load is ended by the test
			break
		}
	}
	_, dvrs := r.pms.state()
	require.Len(t, dvrs, 1)
	assert.Len(t, dvrs[0].Devices, 3)
	assert.NotEmpty(t, r.pms.takeCancelled(), "PMS's loads were cancelled")
	writes := r.pms.takeWrites()
	assert.Equal(t, 0, count(writes, "POST /livetv/dvrs/"), "no reload")
	wants := built.take()
	require.NotEmpty(t, wants)
	for _, w := range wants {
		assert.Equal(t, dvrs[0].UUID+" http://"+address+"/livetv/media/polutv/xmltv.xml whole-1", w)
	}
}
