package livetv_test

import (
	"fmt"
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
		Namespace: ns, Name: "polutv", UID: "uid-1", Ready: true, Enabled: true, EPGSource: "XEPG",
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
