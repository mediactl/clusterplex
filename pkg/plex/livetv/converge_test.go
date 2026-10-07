package livetv_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	"github.com/mediactl/clusterplex/pkg/plex/livetv"
)

const ns = "clustarr-system"

type events struct {
	mu  sync.Mutex
	all []livetv.Event
}

func (e *events) record(ev livetv.Event) {
	e.mu.Lock()
	e.all = append(e.all, ev)
	e.mu.Unlock()
}

func (e *events) reasons() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, ev := range e.all {
		out = append(out, ev.Reason)
	}
	return out
}

type rig struct {
	pms    *fakePMS
	leases *fakeLeases
	ev     *events
	conv   *livetv.Converger
}

func newRig(t *testing.T) *rig {
	pms, client := newFakePMS(t)
	r := &rig{pms: pms, leases: newFakeLeases(), ev: &events{}}
	r.conv = &livetv.Converger{PMS: client, Identity: "machine-a", Leases: r.leases, Record: r.ev.record}
	return r
}

// block is block i of a provider: start 1000+400i, port 47000+i, device id
// B000000i, its PMS device id registered with the fake.
func (r *rig) block(i int) livetv.Block {
	id := fmt.Sprintf("B000000%d", i)
	port := int32(47000 + i)
	r.pms.byPort[fmt.Sprint(port)] = id
	start := fmt.Sprint(1000 + 400*i)
	return livetv.Block{
		Start: start, DeviceID: id, FriendlyName: "polutv " + start + "+", GuidePath: "/xmltv.xml",
		LineupHash: "l" + start, GuideHash: "g" + start, Port: port,
	}
}

func provider(blocks ...livetv.Block) livetv.Provider {
	return livetv.Provider{
		Namespace: ns, Name: "polutv", UID: "uid-1", Ready: true, Enabled: true, EPGSource: "XEPG",
		Address: "livetv.clustarr-system.svc:80", Blocks: blocks,
	}
}

// converge runs passes, ending each guide load PMS started, until one
// converges; it fails after max passes.
func (r *rig) converge(t *testing.T, max int, live ...livetv.Provider) livetv.Result {
	t.Helper()
	var all livetv.Result
	for range max {
		res, err := r.conv.Run(t.Context(), live)
		require.NoError(t, err)
		all.DVRChanged = all.DVRChanged || res.DVRChanged
		all.Changed = append(all.Changed, res.Changed...)
		r.pms.endLoad()
		if res.Status == livetv.Converged {
			all.Status = livetv.Converged
			return all
		}
	}
	t.Fatalf("no convergence in %d passes", max)
	return all
}

func count(writes []string, prefix string) int {
	n := 0
	for _, w := range writes {
		if strings.HasPrefix(w, prefix) {
			n++
		}
	}
	return n
}

func TestThreeBlocksGetALeaseADeviceOnTheLinkLocalPortAndADVREach(t *testing.T) {
	r := newRig(t)
	p := provider(r.block(0), r.block(1), r.block(2))

	res := r.converge(t, 8, p)
	writes := r.pms.takeWrites()
	assert.True(t, res.DVRChanged)
	require.NotEmpty(t, res.Changed, "the provider whose DVRs changed")
	assert.Equal(t, "polutv", res.Changed[0].Name)

	devs, dvrs := r.pms.state()
	require.Len(t, devs, 3)
	require.Len(t, dvrs, 3)
	for i, d := range devs {
		assert.Equal(t, fmt.Sprintf("http://169.254.47.1:%d", 47000+i), d.URI)
	}
	for i, d := range dvrs {
		start := fmt.Sprint(1000 + 400*i)
		assert.Equal(t, fmt.Sprintf("lineup://tv.plex.providers.epg.xmltv/http://169.254.47.1:%d/xmltv.xml#polutv %s+", 47000+i, start), d.Lineup)
		assert.Equal(t, "eng", d.Language)
	}
	assert.Equal(t, 3, count(writes, "POST /media/grabbers/devices"))
	assert.Equal(t, 3, count(writes, "POST /livetv/dvrs")-count(writes, "POST /livetv/dvrs/"), "three DVRs")
	for _, d := range devs {
		assert.Equal(t, 1, r.pms.saves[string(d.Key)], "one map save for device %s", d.DeviceID)
	}
	assert.Equal(t, 3, count(writes, "POST /livetv/dvrs/"), "one guide reload per block")
	for i := range 3 {
		k := fmt.Sprintf("%s/livetv-b000000%d", ns, i)
		assert.Equal(t, "machine-a", r.leases.holder[k])
		assert.Equal(t, map[string]string{livetv.LabelProvider: "polutv", livetv.LabelBlock: fmt.Sprint(1000 + 400*i)}, r.leases.labels[k])
	}

	t.Run("a second pass writes nothing", func(t *testing.T) {
		res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
		require.NoError(t, err)
		assert.Equal(t, livetv.Converged, res.Status)
		assert.False(t, res.DVRChanged)
		assert.Empty(t, r.pms.takeWrites())
	})

	t.Run("a block's new lineup hash saves only that block's map", func(t *testing.T) {
		p.Blocks[1].LineupHash = "changed"
		res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
		require.NoError(t, err)
		assert.Equal(t, livetv.Converged, res.Status)
		assert.Equal(t, []string{"PUT /media/grabbers/devices/" + string(devs[1].Key) + "/channelmap"}, r.pms.takeWrites())
	})

	t.Run("a new block adds one device and one DVR", func(t *testing.T) {
		p.Blocks = append(p.Blocks, r.block(3))
		res := r.converge(t, 4, p)
		writes := r.pms.takeWrites()
		assert.True(t, res.DVRChanged)
		assert.Equal(t, 1, count(writes, "POST /media/grabbers/devices"))
		devs, dvrs := r.pms.state()
		assert.Len(t, devs, 4)
		assert.Len(t, dvrs, 4)
	})

	t.Run("a removed block's DVR and device are deleted", func(t *testing.T) {
		gone := p.Blocks[3]
		p.Blocks = p.Blocks[:3]
		res := r.converge(t, 2, p)
		assert.True(t, res.DVRChanged)
		devs, dvrs := r.pms.state()
		assert.Len(t, devs, 3)
		assert.Len(t, dvrs, 3)
		for _, d := range devs {
			assert.NotEqual(t, gone.DeviceID, d.DeviceID)
		}
		writes := r.pms.takeWrites()
		require.Len(t, writes, 2)
		assert.True(t, strings.HasPrefix(writes[0], "DELETE /livetv/dvrs/"), "the DVR goes first: %v", writes)
		assert.True(t, strings.HasPrefix(writes[1], "DELETE /media/grabbers/devices/"))
	})
}

func TestNothingIsDeletedSavedOrReloadedWhileAGuideLoads(t *testing.T) {
	r := newRig(t)
	p := provider(r.block(0), r.block(1))
	r.converge(t, 6, p)
	r.pms.takeWrites()

	r.pms.startLoad()
	p.Blocks[0].LineupHash, p.Blocks[0].GuideHash = "new", "new"
	removed := p.Blocks[1]
	p.Blocks = p.Blocks[:1]
	res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, livetv.Pending, res.Status)
	assert.False(t, res.DVRChanged)
	assert.Empty(t, r.pms.takeWrites(), "no delete, save or reload while provider.epg.load runs")
	assert.Contains(t, r.ev.reasons(), livetv.ReasonWaitingForGuide)
	devs, _ := r.pms.state()
	assert.Len(t, devs, 2, "block %s's device waits", removed.Start)

	r.pms.endLoad()
	res = r.converge(t, 3, p)
	assert.True(t, res.DVRChanged)
	devs, dvrs := r.pms.state()
	assert.Len(t, devs, 1)
	assert.Len(t, dvrs, 1)
}

func TestABlockAnotherInstallationHoldsIsLeftAlone(t *testing.T) {
	r := newRig(t)
	p := provider(r.block(0))
	r.leases.holder[ns+"/livetv-b0000000"] = "machine-b"

	res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, livetv.Converged, res.Status)
	assert.Empty(t, r.pms.takeWrites())
	assert.Equal(t, []string{livetv.ReasonBlockOwned}, r.ev.reasons())

	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Len(t, r.ev.reasons(), 1, "said once")

	r.leases.expired[ns+"/livetv-b0000000"] = true
	r.converge(t, 4, p)
	devs, dvrs := r.pms.state()
	assert.Len(t, devs, 1, "an expired Lease is taken")
	assert.Len(t, dvrs, 1)
	assert.Equal(t, "machine-a", r.leases.holder[ns+"/livetv-b0000000"])
}

func TestADeviceIDAtAnotherURIIsReportedAndNotTouched(t *testing.T) {
	r := newRig(t)
	b := r.block(0)
	r.pms.devices = append(r.pms.devices, plexapi.Device{Key: "90", UUID: "device://tv.plex.grabbers.hdhomerun/" + b.DeviceID, URI: "http://10.0.0.9:5004", DeviceID: b.DeviceID})

	res, err := r.conv.Run(t.Context(), []livetv.Provider{provider(b)})
	require.NoError(t, err)
	assert.Equal(t, livetv.Converged, res.Status)
	assert.Empty(t, r.pms.takeWrites())
	assert.Equal(t, []string{livetv.ReasonDeviceIDTaken}, r.ev.reasons())
}

func TestGhostsGoAndARealHDHomeRunStays(t *testing.T) {
	r := newRig(t)
	realDev, realDVR := r.pms.addReal()
	r.pms.dvrs = append(r.pms.dvrs, plexapi.DVR{Key: "70", Lineup: "lineup://tv.plex.providers.epg.xmltv/http://169.254.47.1:47009/xmltv.xml#old"})
	p := provider(r.block(0))

	r.converge(t, 4, p)
	devs, dvrs := r.pms.state()
	assert.Contains(t, devs, realDev)
	var keys []string
	for _, d := range dvrs {
		keys = append(keys, string(d.Key))
	}
	assert.Contains(t, keys, string(realDVR.Key), "the real DVR stays")
	assert.NotContains(t, keys, "70", "the ghost goes")
	assert.Len(t, dvrs, 2)

	t.Run("a gone provider's rows go, the real ones stay", func(t *testing.T) {
		r.converge(t, 2)
		devs, dvrs := r.pms.state()
		assert.Equal(t, []plexapi.Device{realDev}, devs)
		require.Len(t, dvrs, 1)
		assert.Equal(t, realDVR.Key, dvrs[0].Key)
	})
}

func TestANotReadyProviderIsLeftAsItIs(t *testing.T) {
	r := newRig(t)
	p := provider(r.block(0))
	r.converge(t, 4, p)
	r.pms.takeWrites()

	p.Ready = false
	p.Blocks[0].LineupHash = "new"
	res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, livetv.Converged, res.Status)
	assert.Empty(t, r.pms.takeWrites(), "neither saved nor deleted")
	devs, _ := r.pms.state()
	assert.Len(t, devs, 1)
}

func TestAFailedPassRetriesWithoutDuplicates(t *testing.T) {
	r := newRig(t)
	p := provider(r.block(0))
	r.pms.failOnce("POST", "/livetv/dvrs", 500)

	res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.Error(t, err)
	assert.Equal(t, livetv.Pending, res.Status)
	assert.Contains(t, r.ev.reasons(), livetv.ReasonFailed)

	r.converge(t, 4, p)
	devs, dvrs := r.pms.state()
	assert.Len(t, devs, 1, "the device is not registered twice")
	assert.Len(t, dvrs, 1, "one DVR")
}

func TestAChannelMapOverPMSsBudgetIsReportedNotSent(t *testing.T) {
	r := newRig(t)
	r.pms.channels = 500
	p := provider(r.block(0))
	var err error
	for range 3 {
		_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
		r.pms.endLoad()
	}
	require.ErrorIs(t, err, plexapi.ErrChannelMapTooLarge)
	assert.Contains(t, r.ev.reasons(), livetv.ReasonChannelMapTooBig)
	assert.Equal(t, 0, count(r.pms.takeWrites(), "PUT "))
}
