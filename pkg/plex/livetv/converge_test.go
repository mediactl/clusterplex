package livetv_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	"github.com/mediactl/clusterplex/pkg/plex/livetv"
)

const ns = "clustarr-system"

// address is the livetv Service as clustarr publishes it in status.address.
const address = "clustarr-livetv.clustarr-system.svc.cluster.local:80"

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

// block is block i of a provider: start 1000+400i, device id B000000i, at
// /hdhr/B000000i on the livetv Service.
func (r *rig) block(i int) livetv.Block {
	id := fmt.Sprintf("B000000%d", i)
	start := fmt.Sprint(1000 + 400*i)
	return livetv.Block{
		Start: start, DeviceID: id, FriendlyName: "polutv " + start + "+", GuidePath: "/xmltv.xml",
		LineupHash: "l" + start, GuideHash: "g" + start, Path: "/hdhr/" + id, URI: "http://" + address + "/hdhr/" + id,
	}
}

func provider(blocks ...livetv.Block) livetv.Provider {
	return livetv.Provider{
		Namespace: ns, Name: "polutv", UID: "uid-1", Ready: true, Enabled: true,
		Address: address, Blocks: blocks,
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

func TestThreeBlocksGetALeaseADeviceAtItsURIAndADVREach(t *testing.T) {
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
		assert.Equal(t, p.Blocks[i].URI, d.URI, "registered at the block's URI on the livetv Service")
	}
	for i, d := range dvrs {
		start := fmt.Sprint(1000 + 400*i)
		assert.Equal(t, fmt.Sprintf("lineup://tv.plex.providers.epg.xmltv/http://%s/hdhr/B000000%d/xmltv.xml#polutv %s+", address, i, start), d.Lineup)
		assert.Equal(t, "eng", d.Language)
	}
	assert.Equal(t, 3, count(writes, "POST /media/grabbers/devices"))
	assert.Equal(t, 3, count(writes, "POST /livetv/dvrs")-count(writes, "POST /livetv/dvrs/"), "three DVRs")
	for _, d := range devs {
		assert.Equal(t, 1, r.pms.saves[string(d.Key)], "one map save for device %s", d.DeviceID)
	}
	assert.Equal(t, 0, count(writes, "POST /livetv/dvrs/"), "no reload: each save's rebuild loaded the guide")
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
		r.pms.endLoad()
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
	assert.Equal(t, livetv.Waiting, res.Status, "only waiting: the loop polls soon")
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

// TestADeviceAtAStaleURIIsReplaced: a PMS device carrying a block's
// DeviceID at another URI -- the link-local proxies' address before the
// direct-tuners cutover, a renamed release -- is clustarr's own at a stale
// address: its DVRs and the device go, then the block is registered at its
// URI with a DVR, its map saved again (the direct-tuners design §4.2, §4.3).
func TestADeviceAtAStaleURIIsReplaced(t *testing.T) {
	r := newRig(t)
	b := r.block(0)
	stale := plexapi.Device{Key: "29", UUID: "device://tv.plex.grabbers.hdhomerun/" + b.DeviceID, URI: "http://169.254.47.1:47000", DeviceID: b.DeviceID, ParentID: 30}
	r.pms.devices = append(r.pms.devices, stale)
	r.pms.dvrs = append(r.pms.dvrs, plexapi.DVR{Key: "30", Lineup: "lineup://tv.plex.providers.epg.xmltv/http://169.254.47.1:47000/xmltv.xml#polutv 1000+", Devices: []plexapi.Device{stale}})
	p := provider(b)

	res := r.converge(t, 6, p)
	writes := r.pms.takeWrites()
	assert.True(t, res.DVRChanged)
	require.GreaterOrEqual(t, len(writes), 3)
	assert.Equal(t, []string{"DELETE /livetv/dvrs/30", "DELETE /media/grabbers/devices/29"}, writes[:2], "the DVR first, then the device")
	devs, dvrs := r.pms.state()
	require.Len(t, devs, 1)
	assert.Equal(t, b.URI, devs[0].URI)
	require.Len(t, dvrs, 1)
	assert.Equal(t, "lineup://tv.plex.providers.epg.xmltv/"+b.URI+"/xmltv.xml#polutv 1000+", dvrs[0].Lineup)
	assert.Equal(t, 1, r.pms.saves[string(devs[0].Key)], "the new DVR's map is saved")
	assert.Equal(t, 1, count(r.ev.reasons(), livetv.ReasonReplaced))
	assert.Equal(t, "machine-a", r.leases.holder[ns+"/livetv-b0000000"], "the Lease is kept through the replacement")
}

// TestADeadDeviceIsDiscoveredAgain: a device PMS marked dead (its refresh
// failed once: on 2026-11-04 the tuner's first render after a manager
// restart had not served the device's path yet) is asked about again, once
// a minute while it stays dead, since PMS never retries it and shows its
// DVR as "Device not found".
func TestADeadDeviceIsDiscoveredAgain(t *testing.T) {
	r := newRig(t)
	b := r.block(0)
	p := provider(b)
	r.converge(t, 4, p)
	r.pms.takeWrites()

	r.pms.mu.Lock()
	r.pms.devices[0].Status = "dead"
	r.pms.mu.Unlock()
	r.converge(t, 2, p)
	writes := r.pms.takeWrites()
	assert.Equal(t, 1, count(writes, "POST /media/grabbers/devices/discover"), "%v", writes)
	devs, _ := r.pms.state()
	require.Len(t, devs, 1)
	assert.Equal(t, "alive", devs[0].Status)
	assert.Equal(t, 1, count(r.ev.reasons(), livetv.ReasonRevived))
	assert.Zero(t, count(writes, "DELETE"), "the device and its DVR stay")

	r.converge(t, 2, p)
	assert.Zero(t, count(r.pms.takeWrites(), "POST /media/grabbers/devices/discover"), "alive again: not asked")
}

// TestADeviceADiscoverLeftDeadIsAskedAboutAgainInAMinute: PMS answers a
// discover sent before the tuner serves the device's path and marks the
// device dead again. The pass is Waiting, not Converged, so the loop runs
// the next in seconds and asks again once reviveEvery has passed; on talos
// (2026-11-10, rev 231) such a pass read as converged and Live TV was off
// for the ten minutes to the next.
func TestADeviceADiscoverLeftDeadIsAskedAboutAgainInAMinute(t *testing.T) {
	r := newRig(t)
	clock := time.Date(2026, 11, 10, 7, 57, 48, 0, time.UTC)
	r.conv.Now = func() time.Time { return clock }
	b := r.block(0)
	p := provider(b)
	r.converge(t, 4, p)
	r.pms.takeWrites()

	r.pms.mu.Lock()
	r.pms.devices[0].Status = "dead"
	r.pms.discoversLeaveDead = 1
	r.pms.mu.Unlock()
	pass := func() livetv.Status {
		t.Helper()
		res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
		require.NoError(t, err)
		return res.Status
	}

	assert.Equal(t, livetv.Waiting, pass(), "the discover left the device dead")
	assert.Equal(t, 1, count(r.pms.takeWrites(), "POST /media/grabbers/devices/discover"))

	clock = clock.Add(20 * time.Second)
	assert.Equal(t, livetv.Waiting, pass(), "still dead")
	assert.Zero(t, count(r.pms.takeWrites(), "POST /media/grabbers/devices/discover"), "not again within reviveEvery")

	clock = clock.Add(time.Minute)
	assert.Equal(t, livetv.Waiting, pass(), "the pass that asked again saw the device dead")
	assert.Equal(t, 1, count(r.pms.takeWrites(), "POST /media/grabbers/devices/discover"), "asked again after reviveEvery")
	devs, _ := r.pms.state()
	require.Len(t, devs, 1)
	assert.Equal(t, "alive", devs[0].Status)

	clock = clock.Add(20 * time.Second)
	assert.Equal(t, livetv.Converged, pass(), "alive: converged")
	assert.Zero(t, count(r.pms.takeWrites(), "POST"), "nothing more to do")
	assert.Equal(t, 2, count(r.ev.reasons(), livetv.ReasonRevived))
}

// TestAnOldDeviceStaysWhilePMSCannotReachTheNewOne: a block that moved to
// a new device (argon-tv onto Plex's own guide, 2026-11-04) is named in the
// status before the tuner serves the device, and PMS answers not found to
// its registration. The old device and its DVR stay until the new one is
// in, rather than leaving Plex without Live TV in between.
func TestAnOldDeviceStaysWhilePMSCannotReachTheNewOne(t *testing.T) {
	r := newRig(t)
	old := r.block(0)
	r.converge(t, 4, provider(old))
	r.pms.takeWrites()

	moved := r.block(1)
	moved.Start, moved.FriendlyName = old.Start, old.FriendlyName
	r.pms.failOnce("POST", "/media/grabbers/devices", http.StatusNotFound)
	res, err := r.conv.Run(t.Context(), []livetv.Provider{provider(moved)})
	require.Error(t, err, "the block's add failed")
	r.pms.endLoad()
	assert.Equal(t, livetv.Pending, res.Status)
	assert.Zero(t, count(r.pms.takeWrites(), "DELETE"), "the old device and its DVR stay while the new one cannot be added")
	devs, dvrs := r.pms.state()
	require.Len(t, devs, 1)
	assert.Equal(t, old.URI, devs[0].URI)
	require.Len(t, dvrs, 1)

	r.converge(t, 6, provider(moved))
	devs, dvrs = r.pms.state()
	require.Len(t, devs, 1)
	assert.Equal(t, moved.URI, devs[0].URI, "once the new device is in, the old one goes")
	require.Len(t, dvrs, 1)
}

// TestADeviceStoredAtAnotherURIIsNotReplacedAgain: a PMS that keeps a
// registered device under another form of its URI (an address resolved, a
// path rewritten) must not have it deleted and registered again on every
// pass -- each loop would delete the DVR and its recording rules. What PMS
// stored is noted on the block's Lease, so a restarted converger, or the
// next lease holder, keeps it too.
func TestADeviceStoredAtAnotherURIIsNotReplacedAgain(t *testing.T) {
	r := newRig(t)
	r.pms.storeAs = func(uri string) string {
		return strings.Replace(uri, address, "10.43.157.248", 1)
	}
	p := provider(r.block(0))
	r.converge(t, 4, p)
	r.pms.takeWrites()
	warned := 0
	for _, e := range r.ev.all {
		if e.Reason == livetv.ReasonStoredElsewhere && e.Warning {
			warned++
		}
	}
	assert.Equal(t, 1, warned, "said once, as a warning")

	for range 3 {
		_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
		require.NoError(t, err)
		r.pms.endLoad()
	}
	assert.Zero(t, count(r.pms.takeWrites(), "DELETE"), "kept as PMS stored it")

	restarted := &livetv.Converger{PMS: r.conv.PMS, Identity: "machine-a", Leases: r.leases, Record: r.ev.record}
	_, err := restarted.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	r.pms.endLoad()
	assert.Zero(t, count(r.pms.takeWrites(), "DELETE"), "a restarted converger reads the note on the Lease")
	devs, dvrs := r.pms.state()
	assert.Len(t, devs, 1)
	assert.Len(t, dvrs, 1)
}

// TestABlockWithoutAURIIsSkippedAndRecorded: a provider an older clustarr
// wrote, or one whose address is not published yet, registers nothing and
// deletes nothing.
func TestABlockWithoutAURIIsSkippedAndRecorded(t *testing.T) {
	r := newRig(t)
	b := r.block(0)
	b.URI = ""
	res, err := r.conv.Run(t.Context(), []livetv.Provider{provider(b)})
	require.Error(t, err)
	assert.Equal(t, livetv.Pending, res.Status)
	assert.Empty(t, r.pms.takeWrites(), "no device added, nothing deleted")
	require.Equal(t, []string{livetv.ReasonFailed}, r.ev.reasons())
	assert.Contains(t, r.ev.all[0].Message, "block 1000")
}

// TestSameURLImpliesThePortAndATrailingSlash: PMS may store a device URI
// with :80 spelled out or a trailing slash.
func TestSameURLImpliesThePortAndATrailingSlash(t *testing.T) {
	assert.True(t, livetv.SameURL("http://a.svc/hdhr/X", "http://a.svc:80/hdhr/X/"))
	assert.True(t, livetv.SameURL("http://A.svc:80/hdhr/X", "http://a.svc/hdhr/X"))
	assert.False(t, livetv.SameURL("http://a.svc/hdhr/X", "http://a.svc/hdhr/Y"))
	assert.False(t, livetv.SameURL("http://a.svc/hdhr/X", "https://a.svc/hdhr/X"))
	assert.False(t, livetv.SameURL("http://a.svc/hdhr/X", "http://a.svc:8080/hdhr/X"))
}

// TestGhostsAreDVRsUnderTheServiceWithNoDevice: a DVR whose guide is under
// the livetv Service's /hdhr/ and which lists no device PMS has is a ghost;
// a DVR elsewhere and a real HDHomeRun stay.
func TestGhostsAreDVRsUnderTheServiceWithNoDevice(t *testing.T) {
	r := newRig(t)
	realDev, realDVR := r.pms.addReal()
	r.pms.dvrs = append(r.pms.dvrs,
		plexapi.DVR{Key: "70", Lineup: "lineup://tv.plex.providers.epg.xmltv/http://" + address + "/hdhr/DEADBEEF/xmltv.xml#old"},
		plexapi.DVR{Key: "71", Lineup: "lineup://tv.plex.providers.epg.xmltv/http://10.0.0.5/xmltv.xml#someone else's"},
	)
	p := provider(r.block(0))

	r.converge(t, 4, p)
	devs, dvrs := r.pms.state()
	assert.Contains(t, devs, realDev)
	var keys []string
	for _, d := range dvrs {
		keys = append(keys, string(d.Key))
	}
	assert.Contains(t, keys, string(realDVR.Key), "the real DVR stays")
	assert.Contains(t, keys, "71", "a DVR whose guide is elsewhere stays")
	assert.NotContains(t, keys, "70", "the ghost goes")
	assert.Len(t, dvrs, 3)

	t.Run("a gone provider's rows go, the real ones stay", func(t *testing.T) {
		// The Service's address was seen while the provider lived.
		r.converge(t, 2)
		devs, dvrs := r.pms.state()
		assert.Equal(t, []plexapi.Device{realDev}, devs)
		require.Len(t, dvrs, 2)
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

func TestASavedMapIsAGuideLoadSoNothingElseFollowsItInThePass(t *testing.T) {
	// A save rebuilds the guide (recorded): the pass that saves block 0's
	// new map must not go on to delete removed block 1's DVR -- the
	// recorded deadlock -- nor reload the guide that rebuild reads.
	r := newRig(t)
	p := provider(r.block(0), r.block(1))
	r.converge(t, 8, p)
	r.pms.takeWrites()

	p.Blocks[0].LineupHash, p.Blocks[0].GuideHash = "new", "new"
	p.Blocks = p.Blocks[:1]
	res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.NotEqual(t, livetv.Converged, res.Status, "the removal waits for the save's load")
	writes := r.pms.takeWrites()
	require.Len(t, writes, 1, "%v", writes)
	assert.True(t, strings.HasSuffix(writes[0], "/channelmap"), "%v", writes)

	r.pms.endLoad()
	res = r.converge(t, 2, p)
	assert.True(t, res.DVRChanged)
	assert.Equal(t, 0, count(r.pms.takeWrites(), "POST /livetv/dvrs/"), "no reload after the save")
}

func TestAGuideLoadPMSStartsMidPassPostponesTheDelete(t *testing.T) {
	// A pass runs for minutes and PMS refreshes guides on its own, so the
	// look at /activities the pass began with is not enough for a delete.
	r := newRig(t)
	p := provider(r.block(0), r.block(1))
	r.converge(t, 8, p)
	r.pms.takeWrites()

	p.Blocks = p.Blocks[:1]
	r.pms.startLoadAfter(1)
	res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.NotEqual(t, livetv.Converged, res.Status)
	assert.Empty(t, r.pms.takeWrites(), "the delete asked PMS again and waited")
}

func TestOneGuideLoadIsStartedPerPass(t *testing.T) {
	r := newRig(t)
	p := provider(r.block(0), r.block(1), r.block(2))
	_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, 1, count(r.pms.takeWrites(), "POST /livetv/dvrs"), "one DVR, and so one load, per pass")
}

func TestAHeldBlocksLeaseIsRenewedWhileItsProviderIsNotReady(t *testing.T) {
	// Its device stays in this PMS, so its Lease must too: a lapse lets
	// another installation register the block beside it.
	r := newRig(t)
	p := provider(r.block(0))
	r.converge(t, 4, p)
	r.pms.takeWrites()
	k := ns + "/livetv-b0000000"
	before := r.leases.acquired(k)

	p.Ready = false
	_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, before+1, r.leases.acquired(k), "not Ready")

	p.Ready, p.Enabled = true, false
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, before+2, r.leases.acquired(k), "disabled")
	assert.Empty(t, r.pms.takeWrites(), "and nothing in PMS changes")
}

func TestHeldLeasesAreRenewedWhilePMSCannotBeListed(t *testing.T) {
	r := newRig(t)
	p := provider(r.block(0))
	r.converge(t, 4, p)
	k := ns + "/livetv-b0000000"
	before := r.leases.acquired(k)

	r.pms.failOnce("GET", "/media/grabbers/devices", 500)
	_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.Error(t, err)
	assert.Equal(t, before+1, r.leases.acquired(k))
}

func TestABlockAnotherInstallationTookWhileThisPMSHasItIsAWarning(t *testing.T) {
	r := newRig(t)
	p := provider(r.block(0))
	r.converge(t, 4, p)
	r.pms.takeWrites()
	r.leases.holder[ns+"/livetv-b0000000"] = "machine-b" // taken while ours lapsed

	_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	last := r.ev.all[len(r.ev.all)-1]
	assert.Equal(t, livetv.ReasonBlockOwned, last.Reason)
	assert.True(t, last.Warning, "two installations serve one block: %s", last.Message)
	assert.Empty(t, r.pms.takeWrites(), "its DVR stays: deleting it is the owner's call")
}

func TestADeleteAnsweredNotFoundIsAlreadyGone(t *testing.T) {
	// A PMS whose DVR list predates another pod's delete still lists the
	// DVR, and deleting it again answers 404.
	r := newRig(t)
	r.pms.dvrs = append(r.pms.dvrs, plexapi.DVR{Key: "70", Lineup: "lineup://tv.plex.providers.epg.xmltv/http://" + address + "/hdhr/DEADBEEF/xmltv.xml#old"})
	r.pms.failOnce("DELETE", "/livetv/dvrs/70", 404)
	res, err := r.conv.Run(t.Context(), []livetv.Provider{provider()})
	require.NoError(t, err)
	assert.Equal(t, livetv.Converged, res.Status)
}

// staleRig is a rig whose PMS started an hour ago and lists block 0's
// device, its parentID naming DVR 30, but not DVR 30: another pod made it.
func staleRig(t *testing.T) (*rig, livetv.Provider, *time.Time) {
	r := newRig(t)
	started := time.Now().Add(-time.Hour)
	r.conv.PMSStarted = func() time.Time { return started }
	b := r.block(0)
	r.pms.devices = append(r.pms.devices, plexapi.Device{Key: "29", UUID: "device://tv.plex.grabbers.hdhomerun/" + b.DeviceID, URI: b.URI, DeviceID: b.DeviceID, ParentID: 30})
	return r, provider(b), &started
}

func TestADeviceWhoseDVRThisPMSDoesNotListGetsNoSecondDVR(t *testing.T) {
	// Each PMS reads its DVRs once, at its start, but its devices from the
	// database, whose parentID names the DVR.
	r, p, started := staleRig(t)

	res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.True(t, res.RestartSelf, "this PMS restarts to read its DVRs")
	assert.NotEqual(t, livetv.Converged, res.Status)
	assert.Empty(t, r.pms.takeWrites(), "no second DVR")
	assert.Contains(t, r.ev.reasons(), livetv.ReasonDVRListStale)

	res, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.False(t, res.RestartSelf, "asked once")
	assert.Empty(t, r.pms.takeWrites())

	*started = time.Now()
	r.pms.dvrs = append(r.pms.dvrs, plexapi.DVR{Key: "30", Lineup: "lineup://tv.plex.providers.epg.xmltv/" + p.Blocks[0].URI + "/xmltv.xml#polutv 1000+", Devices: []plexapi.Device{r.pms.devices[0]}})
	res = r.converge(t, 3, p)
	assert.False(t, res.DVRChanged, "restarted, it lists DVR 30 and keeps it")
	assert.Equal(t, 0, count(r.pms.takeWrites(), "POST /livetv/dvrs"))
}

func TestAParentIDStillUnlistedAfterARestartNamesAGoneDVR(t *testing.T) {
	r, p, started := staleRig(t)
	res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	require.True(t, res.RestartSelf)

	*started = time.Now()
	r.converge(t, 4, p)
	_, dvrs := r.pms.state()
	assert.Len(t, dvrs, 1, "the restarted PMS lists no DVR 30 either: one is created")
}

func TestANewLeaderSkipsTheSavesTheOldOneMade(t *testing.T) {
	// Every rolling update moves the lease: saving every block's map again
	// would rebuild every guide, one load at a time.
	r := newRig(t)
	p := provider(r.block(0), r.block(1))
	r.converge(t, 8, p)
	r.pms.takeWrites()

	next := &livetv.Converger{PMS: r.conv.PMS, Identity: "machine-a", Leases: r.leases, Record: r.ev.record}
	res, err := next.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, livetv.Converged, res.Status)
	assert.Empty(t, r.pms.takeWrites(), "the Leases say what is saved")

	t.Run("not for a DVR made again", func(t *testing.T) {
		_, dvrs := r.pms.state()
		r.pms.dvrs = dvrs[1:] // block 0's DVR deleted in Plex
		r.pms.devices[0].ParentID = 0
		next := &livetv.Converger{PMS: r.conv.PMS, Identity: "machine-a", Leases: r.leases, Record: r.ev.record}
		for range 3 {
			_, err := next.Run(t.Context(), []livetv.Provider{p})
			require.NoError(t, err)
			r.pms.endLoad()
		}
		assert.Equal(t, 1, count(r.pms.takeWrites(), "PUT "), "the new DVR's map is saved")
	})

	t.Run("nor for another installation's", func(t *testing.T) {
		r.leases.expired[ns+"/livetv-b0000001"] = true
		other := &livetv.Converger{PMS: r.conv.PMS, Identity: "machine-b", Leases: r.leases, Record: r.ev.record}
		for range 2 {
			_, err := other.Run(t.Context(), []livetv.Provider{p})
			require.NoError(t, err)
			r.pms.endLoad()
		}
		assert.Equal(t, 1, count(r.pms.takeWrites(), "PUT "), "a takeover drops the note")
	})
}

// fakeXMLTVGuides records what the converger asks the XMLTV guide builder.
type fakeXMLTVGuides struct {
	mu    sync.Mutex
	wants []string // "<dvr> <url> <hash>"
}

func (f *fakeXMLTVGuides) Want(dvrUUID, guideURL, guideHash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wants = append(f.wants, dvrUUID+" "+guideURL+" "+guideHash)
}

func (f *fakeXMLTVGuides) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.wants
	f.wants = nil
	return w
}

// TestAnXMLTVBlockIsWantedAfterItsMapSaveAndNeverReloaded: with the XMLTV
// guides built by the managers (custom guides design §5.8), PMS's first
// load at the DVR's creation pairs the channel map; once the map is saved
// the rebuild the save starts is cancelled, the builder is asked for the
// block's guide on every pass, and a guide-hash move is the builder's, never
// a reloadGuide. Without the builder the move is a reloadGuide, as before.
func TestAnXMLTVBlockIsWantedAfterItsMapSaveAndNeverReloaded(t *testing.T) {
	r := newRig(t)
	built := &fakeXMLTVGuides{}
	r.conv.XMLTVGuides = built
	b := r.block(0)
	p := provider(b)

	// The DVR's creation loads the guide (PMS's first fill): the builder is
	// not asked before the map is saved.
	res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.NotEqual(t, livetv.Converged, res.Status, "the map waits for the creation's load")
	assert.Empty(t, built.take(), "no build before the map is saved")
	r.pms.endLoad()

	res, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, livetv.Converged, res.Status, "the save's rebuild is cancelled, so nothing waits on it")
	_, dvrs := r.pms.state()
	require.Len(t, dvrs, 1)
	url := b.URI + "/xmltv.xml"
	assert.Equal(t, []string{dvrs[0].UUID + " " + url + " g1000"}, built.take())
	writes := r.pms.takeWrites()
	assert.Equal(t, 1, count(writes, "PUT /media/grabbers/devices/"), "the map is saved")
	assert.Equal(t, 1, count(writes, "DELETE /activities/"), "the rebuild the save started is cancelled")
	assert.Zero(t, count(writes, "POST /livetv/dvrs/"), "no reloadGuide")

	p.Blocks[0].GuideHash = "g-moved"
	res, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, livetv.Converged, res.Status)
	assert.Equal(t, []string{dvrs[0].UUID + " " + url + " g-moved"}, built.take(), "the moved hash is the builder's")
	assert.Zero(t, count(r.pms.takeWrites(), "POST /livetv/dvrs/"), "a guide-hash move posts no reloadGuide")

	t.Run("without the builder a guide-hash move reloads the guide", func(t *testing.T) {
		r := newRig(t)
		p := provider(r.block(0))
		r.converge(t, 6, p)
		r.pms.takeWrites()
		p.Blocks[0].GuideHash = "g-moved"
		_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
		require.NoError(t, err)
		_, dvrs := r.pms.state()
		require.Len(t, dvrs, 1)
		assert.Equal(t, []string{"POST /livetv/dvrs/" + string(dvrs[0].Key) + "/reloadGuide"}, r.pms.takeWrites())
	})
}

// TestAnEmptyXMLTVGuideIsPMSsToFillUntilTheBuilderIsAsked: an XMLTV block's
// empty guide is reloaded by PMS, as without the builder, until the
// converger has asked the builder for it; from then on the builder fills
// it, and a load PMS made would be refused at its rename anyway.
func TestAnEmptyXMLTVGuideIsPMSsToFillUntilTheBuilderIsAsked(t *testing.T) {
	r := newRig(t)
	built := &fakeXMLTVGuides{}
	now := time.Unix(1792540000, 0)
	empty := true
	r.conv.XMLTVGuides = built
	r.conv.Now = func() time.Time { return now }
	r.conv.GuideEmpty = func(context.Context, string) (bool, error) { return empty, nil }
	p := provider(r.block(0))
	run := func() {
		t.Helper()
		_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
		require.NoError(t, err)
		r.pms.endLoad()
	}

	run() // the DVR is created, its load runs
	run() // the map is saved; the empty guide waits for the load the save started
	assert.Empty(t, built.take())
	r.pms.takeWrites()
	run()
	assert.Equal(t, 1, reloads(r.pms.takeWrites()), "PMS fills the empty guide")
	assert.Empty(t, built.take(), "the builder is not asked meanwhile")

	run()
	assert.Len(t, built.take(), 1, "asked once PMS's reload is under way")
	now = now.Add(2 * time.Hour)
	run()
	assert.Zero(t, reloads(r.pms.takeWrites()), "an empty guide once asked for is the builder's")
	assert.Len(t, built.take(), 1)
}

// deviceOf is block b's device in the fake PMS.
func (r *rig) deviceOf(t *testing.T, b livetv.Block) plexapi.Device {
	t.Helper()
	devs, _ := r.pms.state()
	for _, d := range devs {
		if d.DeviceID == b.DeviceID {
			return d
		}
	}
	t.Fatalf("no device %s", b.DeviceID)
	return plexapi.Device{}
}

// hasDVR is whether block b's device has a DVR in the fake PMS.
func (r *rig) hasDVR(b livetv.Block) bool {
	_, dvrs := r.pms.state()
	for _, d := range dvrs {
		for _, dev := range d.Devices {
			if dev.DeviceID == b.DeviceID {
				return true
			}
		}
	}
	return false
}

// TestABlockMadeInAnothersCancelWindowKeepsItsFirstLoad: PMS's activities do
// not say whose guide a load is, so the cancel after block A's map save
// would end whatever load runs -- block B's first, made inside A's window
// (the cutover makes 14 category DVRs one after another), which is what
// pairs B's channel map. While a block's map is not saved, no load is
// cancelled; B's map is saved on its next pass, and then B's own rebuild is
// cancelled.
func TestABlockMadeInAnothersCancelWindowKeepsItsFirstLoad(t *testing.T) {
	r := newRig(t)
	r.conv.XMLTVGuides = &fakeXMLTVGuides{}
	now := time.Unix(1792540000, 0)
	r.conv.Now = func() time.Time { return now } // every pass inside A's window
	a, b := r.block(0), r.block(1)
	p := provider(a, b)
	run := func() {
		t.Helper()
		_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
		require.NoError(t, err)
	}

	run() // A's DVR is made, its first load runs
	r.pms.endLoad()
	for range 4 {
		run()
		if r.hasDVR(b) {
			break
		}
		r.pms.endLoad()
	}
	require.True(t, r.hasDVR(b), "B's DVR is made")
	require.Equal(t, 1, r.pms.saves[string(r.deviceOf(t, a).Key)], "A's map is saved: its window is open")
	bUUID := "device://tv.plex.grabbers.hdhomerun/" + b.DeviceID

	run() // B's first load runs, inside A's window
	assert.Empty(t, r.pms.takeCancelled(), "no load is cancelled while B's map is not saved")
	r.pms.mu.Lock()
	loading, of := r.pms.loading, r.pms.loadOf
	r.pms.mu.Unlock()
	assert.True(t, loading, "B's first load runs on")
	assert.Equal(t, "create "+bUUID, of)

	r.pms.endLoad()
	run()
	bKey := string(r.deviceOf(t, b).Key)
	assert.Equal(t, 1, r.pms.saves[bKey], "B's map is saved on its next pass")
	assert.Equal(t, []string{"save " + bKey}, r.pms.takeCancelled(), "with every map saved, B's rebuild is cancelled")
}

// TestAnotherProvidersBlockMadeInACancelWindowKeepsItsFirstLoad: the
// cancel takes every load PMS runs, so the blocks it waits for are every
// converged provider's, not only the provider whose map save opened the
// window. Provider P saves its map; provider Q makes a DVR in the same pass;
// on P's next pass Q's first load runs on, and Q's map is saved after it.
// Cancelling it left Q's map unsaved for good (PMS matched no channel) with
// nothing to reload its guide.
func TestAnotherProvidersBlockMadeInACancelWindowKeepsItsFirstLoad(t *testing.T) {
	r := newRig(t)
	r.conv.XMLTVGuides = &fakeXMLTVGuides{}
	now := time.Unix(1792540000, 0)
	r.conv.Now = func() time.Time { return now } // every pass inside P's window
	a, b := r.block(0), r.block(1)
	p, q := provider(a), provider(b)
	p.Name, p.UID = "argon", "uid-p" // sorts, and so converges, before q
	run := func() {
		t.Helper()
		_, err := r.conv.Run(t.Context(), []livetv.Provider{q, p})
		require.NoError(t, err)
	}

	run() // P's DVR is made, its first load runs; Q waits for it
	r.pms.endLoad()
	for range 4 {
		run()
		if r.hasDVR(b) {
			break
		}
		r.pms.endLoad()
	}
	require.True(t, r.hasDVR(b), "Q's DVR is made")
	require.Equal(t, 1, r.pms.saves[string(r.deviceOf(t, a).Key)], "P's map is saved: its window is open")
	bUUID := "device://tv.plex.grabbers.hdhomerun/" + b.DeviceID

	run() // Q's first load runs, inside P's window
	assert.Empty(t, r.pms.takeCancelled(), "no load is cancelled while Q's map is not saved")
	r.pms.mu.Lock()
	loading, of := r.pms.loading, r.pms.loadOf
	r.pms.mu.Unlock()
	assert.True(t, loading, "Q's first load runs on")
	assert.Equal(t, "create "+bUUID, of)

	r.pms.endLoad()
	run()
	bKey := string(r.deviceOf(t, b).Key)
	assert.Equal(t, 1, r.pms.saves[bKey], "Q's map is saved on its next pass")
	assert.Equal(t, []string{"save " + bKey}, r.pms.takeCancelled(), "with every map saved, Q's rebuild is cancelled")
}

// TestAProviderLeftAsItIsHoldsNoCancel: a provider left as it is (not
// Ready) still has its blocks' Leases kept, but its saved maps are never
// read back from them, since it is not converged. Its blocks are not the
// cancel's to wait for: on a fresh converger they would read unsaved for
// good, and a renumbering's rebuild of a converged provider's built guide
// would never be cancelled.
func TestAProviderLeftAsItIsHoldsNoCancel(t *testing.T) {
	r := newRig(t)
	r.conv.XMLTVGuides = &fakeXMLTVGuides{}
	a, b := r.block(0), r.block(1)
	p, q := provider(a), provider(b)
	p.Name, p.UID = "argon", "uid-p"
	r.converge(t, 10, p, q)

	// A new holder, the same PMS and Leases: q is not Ready.
	r.conv = &livetv.Converger{PMS: r.conv.PMS, Identity: "machine-a", Leases: r.leases, Record: r.ev.record, XMLTVGuides: &fakeXMLTVGuides{}}
	q.Ready = false
	_, err := r.conv.Run(t.Context(), []livetv.Provider{p, q}) // q's Lease is kept
	require.NoError(t, err)
	r.pms.endLoad()
	p.Blocks[0].LineupHash = "renumbered"
	r.pms.takeCancelled()
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p, q})
	require.NoError(t, err)
	aKey := string(r.deviceOf(t, a).Key)
	assert.Equal(t, 2, r.pms.saves[aKey])
	assert.Equal(t, []string{"save " + aKey}, r.pms.takeCancelled(), "the left provider's block holds no cancel")
}

// TestWithEveryMapSavedARenumberingsLoadIsCancelled: in the steady state the
// cancel stays: a renumbering (a lineup-hash move) saves the block's map,
// and the whole XMLTV load PMS starts for it is cancelled.
func TestWithEveryMapSavedARenumberingsLoadIsCancelled(t *testing.T) {
	r := newRig(t)
	r.conv.XMLTVGuides = &fakeXMLTVGuides{}
	p := provider(r.block(0), r.block(1))
	r.converge(t, 10, p)
	r.pms.takeCancelled()

	p.Blocks[1].LineupHash = "renumbered"
	_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	bKey := string(r.deviceOf(t, p.Blocks[1]).Key)
	assert.Equal(t, 2, r.pms.saves[bKey])
	assert.Equal(t, []string{"save " + bKey}, r.pms.takeCancelled())
}
