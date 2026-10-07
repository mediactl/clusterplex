// Package livetv adds each clustarr IPTVProvider to Plex as DVRs (clustarr's
// Live TV spec 2026-10-07 §6.1). clustarr splits a provider's channels into
// blocks of contiguous numbers, because PMS takes at most ~454 channels in
// one device's channel map; each block is one HDHomeRun device at its own
// port on a link-local address every Plex pod serves (proxies.go), and one
// DVR over that block's slice of the guide. The lease holder registers each
// block it holds the Lease for, creates its DVR, saves its channel map and
// reloads its guide when they change. It manages only devices on the
// link-local address or carrying a live block's device id (rule M), and
// never deletes, saves, reloads or creates a DVR while PMS loads a guide: a
// DVR deleted during provider.epg.load deadlocked PMS (recorded). Creating
// a DVR, saving a map and reloading a guide each start such a load, so a
// pass starts at most one, and it asks PMS again before every such write.
package livetv

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

// deviceIDPrefix is an HDHomeRun device's identifier before its device id.
const deviceIDPrefix = "device://tv.plex.grabbers.hdhomerun/"

// DefaultLocalAddress is the link-local address every Plex pod serves the
// blocks' ports on (plex.liveTV.localAddress).
const DefaultLocalAddress = "169.254.47.1"

// The labels on a block's Lease, which clustarr reads to show its owner.
const (
	LabelProvider = "clustarr.io/iptv-provider"
	LabelBlock    = "clustarr.io/iptv-block"
)

// The Event reasons recorded on an IPTVProvider.
const (
	ReasonProvisioned      = "DVRProvisioned"
	ReasonChannelMapSaved  = "DVRChannelMapSaved"
	ReasonNeedsLineup      = "DVRNeedsLineup"
	ReasonFailed           = "DVRProvisionFailed"
	ReasonRemoved          = "DVRRemoved"
	ReasonBlockOwned       = "DVRBlockOwned"
	ReasonDeviceIDTaken    = "DVRDeviceIDTaken"
	ReasonWaitingForGuide  = "DVRWaitingForGuide"
	ReasonChannelMapTooBig = "DVRChannelMapTooLarge"
	ReasonPodsRestarted    = "DVRPodsRestarted"
	ReasonDVRListStale     = "DVRListStale"
)

// Status is how far a pass got.
type Status string

const (
	Converged Status = "converged"
	// Waiting is a pass that only waited for a guide load: nothing failed.
	Waiting Status = "waiting"
	Pending Status = "pending"
	Errored Status = "error"
)

// Result is a pass's outcome.
type Result struct {
	Status Status
	// DVRChanged is a DVR created or deleted on this pass: every other
	// PMS caches its DVRs from its start, so they must restart.
	DVRChanged bool
	// Changed names the providers whose DVRs changed, for the restart's
	// Event.
	Changed []Provider
	// RestartSelf is this PMS's DVR list found older than a DVR another
	// pod made: this pod's PMS must restart to read it.
	RestartSelf bool
}

// Block is one of a provider's blocks, as clustarr publishes it in
// status.blocks.
type Block struct {
	Start, DeviceID, FriendlyName, GuidePath string
	LineupHash, GuideHash                    string
	Port                                     int32
}

// Provider is an IPTVProvider as the converger needs it.
type Provider struct {
	Namespace, Name, UID string
	Ready, Enabled       bool
	EPGSource            string // "XEPG" | "PMS"
	// Address is the livetv Service every block's proxy targets.
	Address string
	Blocks  []Block
}

func (p Provider) key() string { return p.Namespace + "/" + p.Name }

// LeaseClient takes and renews a block's Lease (leases.go).
type LeaseClient interface {
	// Acquire takes or renews the Lease name in namespace for identity.
	Acquire(ctx context.Context, namespace, name, identity string, labels map[string]string) (Lease, error)
	// Annotate sets annotations on a Lease identity holds.
	Annotate(ctx context.Context, namespace, name, identity string, annotations map[string]string) error
}

// Lease is a block's Lease as Acquire left it.
type Lease struct {
	// Held is identity holding it; Holder names whoever does.
	Held   bool
	Holder string
	// Annotations are the holder's notes on it, none once it changed hands.
	Annotations map[string]string
}

// AnnotationSaved is the note on a block's Lease of what its holder last
// saved into PMS: the DVR, and its lineup and guide hashes (savedNote).
const AnnotationSaved = "livetv.clusterplex.io/saved"

// LeaseName is the Lease cluster-plex holds while it registers a block.
func LeaseName(deviceID string) string { return "livetv-" + strings.ToLower(deviceID) }

// Event is one thing a pass did or failed to do, for a provider.
type Event struct {
	Namespace, Name, UID string
	Reason, Message      string
	Warning              bool
}

// Converger runs passes. It is used from one goroutine at a time.
type Converger struct {
	PMS *plexapi.Client
	// Language is the DVR's guide language. Default "eng".
	Language string
	// LocalAddress is the link-local address the blocks' ports are on.
	// Default DefaultLocalAddress.
	LocalAddress string
	// Identity is this installation's PMS machine identifier, the holder
	// of the blocks' Leases: every pod of one installation shares it.
	Identity string
	Leases   LeaseClient
	// PMSStarted is when this pod's PMS last started, which tells a DVR
	// list it reread from one it has not (nil: never).
	PMSStarted func() time.Time
	Logger     *slog.Logger
	Record     func(Event)

	mu sync.Mutex
	// saved is, by device id, the lineup and guide hashes last sent to PMS.
	// In memory only: a restart saves each map and reloads each guide once.
	saved map[string]savedHashes
	// told dedupes the messages a pass would otherwise repeat every time.
	told map[string]string
	// held is, by device id, the Leases this converger holds, renewed even
	// on a pass that cannot reach PMS: the device is still in PMS, and a
	// lapse would let another installation register the block beside it.
	held map[string]heldLease
	// stale is, by device id, the devices staleList asked a restart for.
	stale map[string]staleDVR
}

type heldLease struct {
	namespace string
	labels    map[string]string
}

// staleDVR is a device whose DVR this PMS did not list, and when this
// converger asked for its PMS's restart.
type staleDVR struct {
	parent int
	at     time.Time
}

type savedHashes struct{ lineup, guide string }

// savedNote is AnnotationSaved's value: what was saved, and for which DVR,
// so a DVR made again is saved again.
type savedNote struct {
	DVR    string `json:"dvr"`
	Lineup string `json:"lineup"`
	Guide  string `json:"guide"`
}

func (c *Converger) record(p Provider, reason, msg string, warning bool) {
	if c.Logger != nil {
		lvl := slog.LevelInfo
		if warning {
			lvl = slog.LevelWarn
		}
		c.Logger.Log(context.Background(), lvl, "livetv: "+msg, "provider", p.key(), "reason", reason)
	}
	if c.Record != nil {
		c.Record(Event{Namespace: p.Namespace, Name: p.Name, UID: p.UID, Reason: reason, Message: msg, Warning: warning})
	}
}

// recordOnce records msg under key until a different msg replaces it.
func (c *Converger) recordOnce(key string, p Provider, reason, msg string, warning bool) {
	if c.told[key] == msg {
		return
	}
	c.told[key] = msg
	c.record(p, reason, msg, warning)
}

func (c *Converger) localAddress() string {
	if c.LocalAddress != "" {
		return c.LocalAddress
	}
	return DefaultLocalAddress
}

func (c *Converger) blockURI(b Block) string {
	return "http://" + net.JoinHostPort(c.localAddress(), strconv.Itoa(int(b.Port)))
}

// errWaiting is a step a guide load postponed; the pass is Pending.
var errWaiting = errors.New("waiting for PMS to finish loading a guide")

// idle is errWaiting while PMS loads a guide, this pass's own or one PMS
// started itself. It asks PMS again every time: a pass runs for minutes,
// and the look it began with is stale by the time it deletes.
func (c *Converger) idle(ctx context.Context, ps *pass) error {
	if ps.guideBusy {
		return errWaiting
	}
	acts, err := c.PMS.Activities(ctx)
	if err != nil {
		return fmt.Errorf("list activities: %w", err)
	}
	for _, a := range acts {
		if a.Type == plexapi.ActivityGuideLoad {
			ps.guideBusy = true
			return errWaiting
		}
	}
	return nil
}

// pass is one Run's view of PMS.
type pass struct {
	devices map[string]plexapi.Device // by device id, HDHomeRun only
	dvrs    []plexapi.DVR
	// guideBusy is PMS loading a guide, or this pass having started one.
	guideBusy   bool
	changed     map[string]Provider
	restartSelf bool
}

// Run is one idempotent pass over every live provider (Live TV spec §6.1).
// A block's failure is recorded and makes the pass Pending; the other
// blocks still converge.
func (c *Converger) Run(ctx context.Context, live []Provider) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.saved == nil {
		c.saved, c.told, c.held, c.stale = map[string]savedHashes{}, map[string]string{}, map[string]heldLease{}, map[string]staleDVR{}
	}
	devices, err := c.PMS.Devices(ctx)
	if err != nil {
		c.renewHeld(ctx)
		return Result{Status: Errored}, fmt.Errorf("list devices: %w", err)
	}
	dvrs, err := c.PMS.DVRs(ctx)
	if err != nil {
		c.renewHeld(ctx)
		return Result{Status: Errored}, fmt.Errorf("list DVRs: %w", err)
	}
	acts, err := c.PMS.Activities(ctx)
	if err != nil {
		c.renewHeld(ctx)
		return Result{Status: Errored}, fmt.Errorf("list activities: %w", err)
	}
	ps := &pass{devices: map[string]plexapi.Device{}, dvrs: dvrs, changed: map[string]Provider{}}
	ps.addDevices(devices)
	for _, a := range acts {
		if a.Type == plexapi.ActivityGuideLoad {
			ps.guideBusy = true
		}
	}

	live = slices.Clone(live)
	slices.SortFunc(live, func(a, b Provider) int { return strings.Compare(a.key(), b.key()) })
	named := map[string]bool{} // device ids of every live block
	for _, p := range live {
		for _, b := range p.Blocks {
			if b.DeviceID != "" {
				named[strings.ToUpper(b.DeviceID)] = true
			}
		}
	}

	pending, waiting := false, false
	var errs []error
	fail := func(p Provider, what string, err error) {
		if errors.Is(err, errWaiting) {
			waiting = true
			return
		}
		pending = true
		if p.UID != "" {
			c.record(p, ReasonFailed, what+": "+err.Error(), true)
		}
		errs = append(errs, fmt.Errorf("%s %s: %w", p.key(), what, err))
	}
	for _, p := range live {
		if !p.Ready || !p.Enabled || p.Address == "" {
			// Left as it is, but its devices are still in this PMS: keep
			// their Leases.
			for _, b := range p.Blocks {
				if err := c.keep(ctx, p, b, ps); err != nil {
					fail(p, "block "+b.Start, err)
				}
			}
			continue
		}
		bs := slices.Clone(p.Blocks)
		slices.SortFunc(bs, func(a, b Block) int { return compareNumbers(a.Start, b.Start) })
		for _, b := range bs {
			if b.DeviceID == "" || b.Port == 0 {
				continue
			}
			if err := c.converge(ctx, p, b, ps); err != nil {
				fail(p, "block "+b.Start, err)
			}
		}
	}

	// Managed rows no live block names: a removed block, a gone provider.
	gone := Provider{Name: "(gone)"}
	for id, dev := range ps.devices {
		if named[id] || hostOf(dev.URI) != c.localAddress() {
			continue
		}
		if err := c.remove(ctx, gone, dev, ps); err != nil {
			fail(gone, "device "+dev.UUID, err)
		}
	}
	for _, d := range ps.ghosts(c.localAddress()) {
		if err := c.idle(ctx, ps); err != nil {
			fail(gone, "ghost DVR "+string(d.Key), err)
			continue
		}
		if err := c.deleteDVR(ctx, ps, d.Key); err != nil {
			fail(gone, "ghost DVR "+string(d.Key), err)
			continue
		}
		ps.changed[gone.key()] = gone
		c.log("deleted ghost DVR "+string(d.Key), "lineup", d.Lineup)
	}

	res := Result{Status: Converged, DVRChanged: len(ps.changed) > 0, RestartSelf: ps.restartSelf}
	for _, p := range ps.changed {
		if p.UID != "" {
			res.Changed = append(res.Changed, p)
		}
	}
	slices.SortFunc(res.Changed, func(a, b Provider) int { return strings.Compare(a.key(), b.key()) })
	switch {
	case pending:
		res.Status = Pending
	case waiting:
		res.Status = Waiting
	}
	return res, errors.Join(errs...)
}

func (c *Converger) log(msg string, args ...any) {
	if c.Logger != nil {
		c.Logger.Info("livetv: "+msg, args...)
	}
}

// acquire takes or renews block b's Lease, reporting when another
// installation holds it.
func (c *Converger) acquire(ctx context.Context, p Provider, b Block, ps *pass) (Lease, error) {
	id := strings.ToUpper(b.DeviceID)
	labels := map[string]string{LabelProvider: p.Name, LabelBlock: b.Start}
	lease, err := c.Leases.Acquire(ctx, p.Namespace, LeaseName(id), c.Identity, labels)
	if err != nil {
		return Lease{}, fmt.Errorf("lease %s: %w", LeaseName(id), err)
	}
	if holder := lease.Holder; !lease.Held {
		delete(c.held, id)
		if dev, ok := ps.devices[id]; ok && sameEndpoint(dev.URI, c.blockURI(b)) {
			// Its DVR is left in place: deleting it deletes its
			// recording rules, which is for a person to decide.
			c.recordOnce("owned/"+id, p, ReasonBlockOwned,
				fmt.Sprintf("block %s (device %s) is registered by %s, and this Plex has its device and DVR too: two installations now serve the block. Delete the DVR in one of them", b.Start, id, holder), true)
			return lease, nil
		}
		c.recordOnce("owned/"+id, p, ReasonBlockOwned,
			fmt.Sprintf("block %s (device %s) is registered by %s; skipping it", b.Start, id, holder), false)
		return lease, nil
	}
	delete(c.told, "owned/"+id)
	c.held[id] = heldLease{namespace: p.Namespace, labels: labels}
	return lease, nil
}

// keep renews the Lease of a block whose provider is left as it is, while
// this PMS has the block's device.
func (c *Converger) keep(ctx context.Context, p Provider, b Block, ps *pass) error {
	if b.DeviceID == "" || b.Port == 0 {
		return nil
	}
	dev, ok := ps.devices[strings.ToUpper(b.DeviceID)]
	if !ok || !sameEndpoint(dev.URI, c.blockURI(b)) {
		return nil
	}
	_, err := c.acquire(ctx, p, b, ps)
	return err
}

// renewHeld renews every Lease this converger holds, on a pass that could
// not read PMS.
func (c *Converger) renewHeld(ctx context.Context) {
	for id, h := range c.held {
		lease, err := c.Leases.Acquire(ctx, h.namespace, LeaseName(id), c.Identity, h.labels)
		if err != nil {
			c.log("renew a block's Lease while PMS cannot be read", "lease", LeaseName(id), "error", err)
			continue
		}
		if !lease.Held {
			delete(c.held, id)
		}
	}
}

// converge brings one block's device, DVR, channel map and guide into PMS.
func (c *Converger) converge(ctx context.Context, p Provider, b Block, ps *pass) error {
	id := strings.ToUpper(b.DeviceID)
	lease, err := c.acquire(ctx, p, b, ps)
	if err != nil || !lease.Held {
		return err
	}

	uri := c.blockURI(b)
	dev, ok := ps.devices[id]
	if !ok {
		if err := c.PMS.AddDevice(ctx, uri); err != nil && !errors.Is(err, plexapi.ErrConflict) {
			return fmt.Errorf("add device %s: %w", uri, err)
		}
		devices, err := c.PMS.Devices(ctx)
		if err != nil {
			return fmt.Errorf("list devices: %w", err)
		}
		ps.addDevices(devices)
		if dev, ok = ps.devices[id]; !ok {
			return fmt.Errorf("PMS lists no device %s after adding %s", id, uri)
		}
		delete(c.saved, id)
		c.record(p, ReasonProvisioned, fmt.Sprintf("block %s: registered device %s at %s", b.Start, dev.UUID, uri), false)
	} else if !sameEndpoint(dev.URI, uri) {
		// Never register it again: that is how duplicate rows arise.
		c.recordOnce("taken/"+id, p, ReasonDeviceIDTaken,
			fmt.Sprintf("block %s: PMS has device %s at %s, not %s; changing nothing", b.Start, dev.UUID, dev.URI, uri), true)
		return nil
	}
	delete(c.told, "taken/"+id)

	dvrs := ps.dvrsOf(dev.UUID)
	if extra := dvrs[min(1, len(dvrs)):]; len(extra) > 0 {
		for _, d := range extra {
			if err := c.idle(ctx, ps); err != nil {
				return c.wait(p, b, "delete duplicate DVRs", err)
			}
			if err := c.deleteDVR(ctx, ps, d.Key); err != nil {
				return fmt.Errorf("delete duplicate DVR %s: %w", d.Key, err)
			}
			ps.changed[p.key()] = p
			c.record(p, ReasonRemoved, fmt.Sprintf("block %s: deleted duplicate DVR %s", b.Start, d.Key), false)
		}
	}
	var dvr plexapi.DVR
	if len(dvrs) > 0 {
		dvr = dvrs[0]
		delete(c.stale, id)
	} else {
		if dev.ParentID != 0 {
			if err := c.staleList(p, b, dev, ps); err != nil {
				return err
			}
		}
		if p.EPGSource == "PMS" {
			c.recordOnce("lineup/"+id, p, ReasonNeedsLineup,
				fmt.Sprintf("block %s: epgSource is PMS: finish device %s's DVR once in Plex's setup wizard, choosing your location's lineup; the channel map is then kept for you", b.Start, id), false)
			return nil
		}
		if err := c.idle(ctx, ps); err != nil {
			return c.wait(p, b, "create the DVR", err)
		}
		lang := c.Language
		if lang == "" {
			lang = "eng"
		}
		created, err := c.PMS.CreateDVR(ctx, lang, dev.UUID, plexapi.XMLTVLineup(uri+b.GuidePath, b.FriendlyName))
		if err != nil {
			return fmt.Errorf("create DVR: %w", err)
		}
		ps.changed[p.key()] = p
		ps.guideBusy = true // creating a DVR loads its guide
		if created.Key == "" {
			all, err := c.PMS.DVRs(ctx)
			if err != nil {
				return fmt.Errorf("list DVRs: %w", err)
			}
			ps.dvrs = all
			got := ps.dvrsOf(dev.UUID)
			if len(got) == 0 {
				return errors.New("PMS lists no DVR on the device after creating one")
			}
			created = got[0]
		}
		dvr = created
		delete(c.saved, id)
		c.record(p, ReasonProvisioned, fmt.Sprintf("block %s: created DVR %s", b.Start, dvr.Key), false)
	}
	delete(c.told, "lineup/"+id)

	s, ok := c.saved[id]
	if n := (savedNote{}); !ok && json.Unmarshal([]byte(lease.Annotations[AnnotationSaved]), &n) == nil && n.DVR == string(dvr.Key) {
		// What the previous holder saved, in this installation's PMS.
		s = savedHashes{lineup: n.Lineup, guide: n.Guide}
		c.saved[id] = s
	}
	if s.lineup != b.LineupHash {
		if err := c.idle(ctx, ps); err != nil {
			return c.wait(p, b, "save the channel map", err)
		}
		m, err := c.PMS.EPGChannelMap(ctx, dev.UUID, dvr.Lineup)
		if err != nil {
			return fmt.Errorf("read the EPG channel map: %w", err)
		}
		if n := plexapi.MapBytes(m); n > plexapi.MaxChannelMapBytes {
			c.recordOnce("toolarge/"+id, p, ReasonChannelMapTooBig,
				fmt.Sprintf("block %s: the channel map of %d channels is %d bytes, over PMS's %d; not saved", b.Start, len(m), n, plexapi.MaxChannelMapBytes), true)
			return fmt.Errorf("%w: %d bytes", plexapi.ErrChannelMapTooLarge, n)
		}
		delete(c.told, "toolarge/"+id)
		if err := c.PMS.SaveChannelMap(ctx, string(dev.Key), m); err != nil {
			if errors.Is(err, plexapi.ErrEmptyChannelMap) {
				c.waiting(p, b, "save the channel map: PMS matched no channel yet")
				return errWaiting
			}
			return fmt.Errorf("save the channel map (%d channels): %w", len(m), err)
		}
		// The save rebuilds the guide (recorded), which reads the block's
		// guide as it is now: no reload follows it.
		ps.guideBusy = true
		s.lineup, s.guide = b.LineupHash, b.GuideHash
		c.saved[id] = s
		c.note(ctx, p, id, dvr.Key, s)
		c.record(p, ReasonChannelMapSaved, fmt.Sprintf("block %s: saved the channel map: %d channels", b.Start, len(m)), false)
	}
	if b.GuideHash != "" && s.guide != b.GuideHash {
		if err := c.idle(ctx, ps); err != nil {
			return c.wait(p, b, "reload the guide", err)
		}
		if err := c.PMS.ReloadGuide(ctx, string(dvr.Key)); err != nil {
			return fmt.Errorf("reload the guide: %w", err)
		}
		ps.guideBusy = true
		s.guide = b.GuideHash
		c.saved[id] = s
		c.note(ctx, p, id, dvr.Key, s)
	}
	delete(c.told, "wait/"+id)
	return nil
}

// staleList holds back a DVR for a device the database already ties to one
// this PMS does not list. Each PMS reads its DVRs once, at its start
// (recorded), so its list predates a DVR another pod made, and a new one
// would be a duplicate. It asks once for this pod's PMS to restart; a
// parentID still unlisted after the restart names a DVR that is gone, and
// the DVR is created.
func (c *Converger) staleList(p Provider, b Block, dev plexapi.Device, ps *pass) error {
	id := strings.ToUpper(b.DeviceID)
	st, asked := c.stale[id]
	if !asked || st.parent != dev.ParentID {
		c.stale[id] = staleDVR{parent: dev.ParentID, at: time.Now()}
		ps.restartSelf = true
		c.record(p, ReasonDVRListStale, fmt.Sprintf("block %s: device %s belongs to DVR %d, which this Plex does not list yet; restarting this pod's Plex to read it rather than creating a second DVR", b.Start, id, dev.ParentID), true)
		return errWaiting
	}
	var started time.Time
	if c.PMSStarted != nil {
		started = c.PMSStarted()
	}
	if !started.After(st.at) {
		return errWaiting
	}
	delete(c.stale, id)
	c.log("a device's DVR is still unlisted after this PMS restarted; creating one", "device", dev.UUID, "parentID", dev.ParentID)
	return nil
}

// note records on the block's Lease what is saved, so the next holder does
// not save it again. A failure costs one save and reload, so it is logged.
func (c *Converger) note(ctx context.Context, p Provider, id string, dvr plexapi.Str, s savedHashes) {
	v, _ := json.Marshal(savedNote{DVR: string(dvr), Lineup: s.lineup, Guide: s.guide})
	if err := c.Leases.Annotate(ctx, p.Namespace, LeaseName(id), c.Identity, map[string]string{AnnotationSaved: string(v)}); err != nil {
		c.log("note the saved channel map on the block's Lease; the next holder saves it again", "lease", LeaseName(id), "error", err)
	}
}

// waiting records, once per step, that a guide load postponed it.
func (c *Converger) waiting(p Provider, b Block, what string) {
	c.recordOnce("wait/"+strings.ToUpper(b.DeviceID), p, ReasonWaitingForGuide,
		fmt.Sprintf("block %s: PMS is loading a guide; will %s once it ends", b.Start, what), false)
}

// wait passes on idle's err, recording the step a guide load postponed.
func (c *Converger) wait(p Provider, b Block, what string, err error) error {
	if errors.Is(err, errWaiting) {
		c.waiting(p, b, what)
	}
	return err
}

// deleteDVR deletes a DVR, a 404 counting as done: a PMS whose DVR list
// predates another pod's delete still lists the DVR.
func (c *Converger) deleteDVR(ctx context.Context, ps *pass, key plexapi.Str) error {
	if err := c.PMS.DeleteDVR(ctx, string(key)); err != nil && !errors.Is(err, plexapi.ErrNotFound) {
		return err
	}
	ps.dropDVR(key)
	return nil
}

// remove deletes a managed device's DVRs, then the device, unless a guide
// is loading.
func (c *Converger) remove(ctx context.Context, p Provider, dev plexapi.Device, ps *pass) error {
	for _, d := range ps.dvrsOf(dev.UUID) {
		if err := c.idle(ctx, ps); err != nil {
			return err
		}
		if err := c.deleteDVR(ctx, ps, d.Key); err != nil {
			return fmt.Errorf("delete DVR %s: %w", d.Key, err)
		}
		ps.changed[p.key()] = p
	}
	if err := c.idle(ctx, ps); err != nil {
		return err
	}
	if err := c.PMS.DeleteDevice(ctx, string(dev.Key)); err != nil && !errors.Is(err, plexapi.ErrNotFound) {
		return fmt.Errorf("delete device %s: %w", dev.Key, err)
	}
	id := deviceID(dev)
	delete(ps.devices, id)
	delete(c.saved, id)
	delete(c.held, id)
	c.log("deleted the DVRs and device of a block no provider names", "device", dev.UUID, "uri", dev.URI)
	return nil
}

// dropDVR forgets a DVR this pass deleted, so no later step of the pass
// deletes it again.
func (ps *pass) dropDVR(key plexapi.Str) {
	ps.dvrs = slices.DeleteFunc(ps.dvrs, func(d plexapi.DVR) bool { return d.Key == key })
}

func (ps *pass) addDevices(devices []plexapi.Device) {
	for _, d := range devices {
		if id := deviceID(d); id != "" {
			ps.devices[id] = d
		}
	}
}

// dvrsOf is the DVRs over device uuid, lowest key first.
func (ps *pass) dvrsOf(uuid string) []plexapi.DVR {
	var out []plexapi.DVR
	for _, d := range ps.dvrs {
		for _, dev := range d.Devices {
			if dev.UUID == uuid {
				out = append(out, d)
				break
			}
		}
	}
	slices.SortFunc(out, func(a, b plexapi.DVR) int { return compareKeys(string(a.Key), string(b.Key)) })
	return out
}

// ghosts are DVRs whose XMLTV guide is on the link-local address but which
// list no device PMS has: a row PMS kept after its device went (recorded).
func (ps *pass) ghosts(local string) []plexapi.DVR {
	var out []plexapi.DVR
	for _, d := range ps.dvrs {
		if lineupHost(d.Lineup) != local {
			continue
		}
		onDevice := false
		for _, dev := range d.Devices {
			if _, ok := ps.devices[deviceID(dev)]; ok {
				onDevice = true
			}
		}
		if !onDevice {
			out = append(out, d)
		}
	}
	return out
}

// lineupHost is the host of an XMLTV lineup id's guide URL.
func lineupHost(lineup string) string {
	rest, ok := strings.CutPrefix(lineup, "lineup://tv.plex.providers.epg.xmltv/")
	if !ok {
		return ""
	}
	if dec, err := url.QueryUnescape(rest); err == nil {
		rest = dec
	}
	rest, _, _ = strings.Cut(rest, "#")
	return hostOf(rest)
}

// deviceID is an HDHomeRun device's id, from its identifier; "" for any
// other kind of device.
func deviceID(d plexapi.Device) string {
	if id, ok := strings.CutPrefix(d.UUID, deviceIDPrefix); ok && id != "" {
		return strings.ToUpper(id)
	}
	return ""
}

func hostOf(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// sameEndpoint compares two device URIs by host and port, port 80 implied.
func sameEndpoint(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return false
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		return "80"
	}
	return ua.Hostname() == ub.Hostname() && port(ua) == port(ub)
}

func compareKeys(a, b string) int {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		return ai - bi
	}
	return strings.Compare(a, b)
}

// compareNumbers orders channel numbers as clustarr's
// xmltv.CompareNumbers does: "9" < "9.2" < "10".
func compareNumbers(a, b string) int {
	am, an := splitNumber(a)
	bm, bn := splitNumber(b)
	if am != bm {
		return cmp.Compare(am, bm)
	}
	if an != bn {
		return cmp.Compare(an, bn)
	}
	return strings.Compare(a, b)
}

func splitNumber(s string) (major, minor int) {
	ma, mi, _ := strings.Cut(s, ".")
	major, _ = strconv.Atoi(ma)
	minor, _ = strconv.Atoi(mi)
	return major, minor
}
