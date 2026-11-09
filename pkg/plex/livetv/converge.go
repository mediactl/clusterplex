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
// guide each start such a load, so a pass starts at most one, and it asks
// PMS again before every such write.
package livetv

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

// The labels on a block's Lease, which clustarr reads to show its owner.
const (
	LabelProvider = "clustarr.io/iptv-provider"
	LabelBlock    = "clustarr.io/iptv-block"
)

// The Event reasons recorded on an IPTVProvider.
const (
	ReasonProvisioned        = "DVRProvisioned"
	ReasonChannelMapSaved    = "DVRChannelMapSaved"
	ReasonFailed             = "DVRProvisionFailed"
	ReasonRemoved            = "DVRRemoved"
	ReasonBlockOwned         = "DVRBlockOwned"
	ReasonReplaced           = "DVRDeviceReplaced"
	ReasonStoredElsewhere    = "DVRDeviceStoredElsewhere"
	ReasonWaitingForGuide    = "DVRWaitingForGuide"
	ReasonChannelMapTooBig   = "DVRChannelMapTooLarge"
	ReasonPodsRestarted      = "DVRPodsRestarted"
	ReasonDVRListStale       = "DVRListStale"
	ReasonRevived            = "DVRDeviceRevived"
	ReasonGuideRefreshed     = "DVRGuideRefreshed"
	ReasonRefreshWaiting     = "DVRGuideRefreshWaiting"
	ReasonChannelMapRestored = "DVRChannelMapRestored"
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
	// Lineup is a Plex block's cloud lineup
	// (lineup://tv.plex.providers.epg.cloud/<id>#<title>): its DVR is
	// created with it, its channel map is the tuner's <uri>/plexmap.json,
	// and its guide is Plex's own, never reloaded. Empty for a block on
	// clustarr's XMLTV guide.
	Lineup string
	// Path is the block's device under its provider's Address,
	// /hdhr/<deviceID>, and URI the device's address PMS registers,
	// http://<Address><Path>. An empty URI registers nothing.
	Path, URI string
}

// Provider is an IPTVProvider as the converger needs it.
type Provider struct {
	Namespace, Name, UID string
	Ready, Enabled       bool
	// Address is the livetv Service, host:port, every block's URI is on.
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

// AnnotationRegistered is the note on a block's Lease of the URI its holder
// registered the device at and the URI PMS lists it under
// (registeredNote): a PMS that keeps another form of the URI must not have
// its device replaced again on every pass, deleting the DVR each time.
const AnnotationRegistered = "livetv.clusterplex.io/registered"

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
	// Identity is this installation's PMS machine identifier, the holder
	// of the blocks' Leases: every pod of one installation shares it.
	Identity string
	Leases   LeaseClient
	// PMSStarted is when this pod's PMS last started, which tells a DVR
	// list it reread from one it has not (nil: never).
	PMSStarted func() time.Time
	Logger     *slog.Logger
	Record     func(Event)
	// GuideEmpty reports a DVR's guide holding no airings; nil skips the
	// check. A guide's schema is empty after its first start
	// (docs/superpowers/specs/2026-10-20-epg-in-postgresql-design.md), and
	// a pod's SQLite guide after its pod is replaced, while the Lease note
	// still says the guide was loaded, so nothing else would reload it.
	GuideEmpty func(ctx context.Context, dvrUUID string) (bool, error)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// CloudRefresh is the UTC hours [start, end) within which a Plex cloud
	// lineup's guide that runs short is reloaded whole (refreshCloudGuide);
	// the zero window never reloads one. The pods run in UTC: 8-11 is
	// 02:00-05:00 US Central standard time.
	CloudRefresh [2]int
	// GuideHorizon reports when a DVR's guide ends, the latest airing's end,
	// read from its schema; the zero time for a guide with no airings. Nil
	// never reloads a cloud guide.
	GuideHorizon func(ctx context.Context, dvrUUID string) (time.Time, error)
	// HTTP fetches a Plex block's channel map from the tuner; nil is a
	// client with a 10 s timeout.
	HTTP *http.Client

	mu sync.Mutex
	// saved is, by device id, the lineup and guide hashes last sent to PMS.
	// In memory only: a restart saves each map and reloads each guide once.
	saved map[string]savedHashes
	// told dedupes the messages a pass would otherwise repeat every time.
	told map[string]string
	// removalsHeld is whether the last pass kept the devices no block
	// names because a block failed to converge, so the hold is logged once.
	removalsHeld bool
	// held is, by device id, the Leases this converger holds, renewed even
	// on a pass that cannot reach PMS: the device is still in PMS, and a
	// lapse would let another installation register the block beside it.
	held map[string]heldLease
	// stale is, by device id, the devices staleList asked a restart for.
	stale map[string]staleDVR
	// revived is, by device id, when PMS was last asked to discover a
	// device it had marked dead again, so a device that stays dead (its
	// tuner down) is asked about once a reviveEvery, not every pass.
	revived map[string]time.Time
	// emptyReloaded is, by device id, when an empty guide was last
	// reloaded: a guide its feed leaves empty is tried again only hourly.
	emptyReloaded map[string]time.Time
	// cloudReloaded is, by device id, when a cloud guide's daily reload
	// was last asked for: PMS moves the DVR's refreshedAt only when the
	// load ends, half an hour later, so the ask is not repeated meanwhile.
	cloudReloaded map[string]time.Time
	// addresses is every livetv Service address a live provider has named
	// since this converger started, so a deleted provider's devices and
	// DVRs, still under its Service, are found and removed.
	addresses map[string]bool
	// registered is, by device id, the URI each block was registered at
	// and the URI PMS lists it under, as AnnotationRegistered notes it.
	registered map[string]registeredNote
	// mapChecked is, by device id, when a Plex block's saved map was last
	// read back from PMS and the DVR's refreshedAt then; mapRestored when
	// a map PMS had replaced was last saved again (restoreMap).
	mapChecked  map[string]mapCheck
	mapRestored map[string]time.Time
}

// mapCheck is one look at a device's saved channel map.
type mapCheck struct {
	at          time.Time
	refreshedAt int64
}

// mapCheckEvery is how often a Plex block's saved map is read back from
// PMS between guide loads; a load's end (the DVR's refreshedAt moving)
// reads it at once, since a load that fails is when PMS replaces it.
const mapCheckEvery = 10 * time.Minute

// mapRestoreEvery bounds how often a replaced map is saved again: every
// save starts a whole guide load (about 30 min for 233 channels), so a PMS
// that replaces the map at every load's end is told once an hour, not
// kept loading.
const mapRestoreEvery = time.Hour

// emptyGuideRetry is how soon an empty guide is reloaded again.
const emptyGuideRetry = time.Hour

// cloudHorizonFloor is how far ahead a cloud guide must reach: PMS's own
// nightly refresh (the RefreshEpgGuides butler task, kept on for Live TV
// since 2026-11-04) extends it two days at a time within a 14-day window,
// so a guide that ends within a week has gone a week without one, and the
// lease holder reloads it whole in the CloudRefresh window.
const cloudHorizonFloor = 7 * 24 * time.Hour

func (c *Converger) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
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

// registeredNote is AnnotationRegistered's value.
type registeredNote struct {
	URI    string `json:"uri"`
	Stored string `json:"stored"`
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
		c.emptyReloaded, c.addresses, c.registered = map[string]time.Time{}, map[string]bool{}, map[string]registeredNote{}
		c.revived, c.cloudReloaded = map[string]time.Time{}, map[string]time.Time{}
		c.mapChecked, c.mapRestored = map[string]mapCheck{}, map[string]time.Time{}
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
		if p.Address != "" {
			c.addresses[p.Address] = true
		}
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
			if b.DeviceID == "" {
				continue
			}
			if err := c.converge(ctx, p, b, ps); err != nil {
				fail(p, "block "+b.Start, err)
			}
		}
	}

	// Managed rows no live block names: a removed block, a gone provider.
	// Not on a pass a block failed on: a block that moved to a new device
	// (argon-tv onto Plex's own guide, 2026-11-04) named it before the
	// tuner served it, PMS could not fetch it ("POST /media/grabbers/devices:
	// plex: not found"), and the old device's DVR was deleted at once, so
	// Plex had no Live TV until the add succeeded 90 s later. The old device
	// and its DVR stay until every block is in.
	gone := Provider{Name: "(gone)"}
	if pending {
		if !c.removalsHeld {
			c.log("a block did not converge; the devices no block names stay until it does")
		}
		c.removalsHeld = true
	} else {
		c.removalsHeld = false
		for id, dev := range ps.devices {
			if named[id] || !c.underService(dev.URI) {
				continue
			}
			if err := c.remove(ctx, gone, dev, ps); err != nil {
				fail(gone, "device "+dev.UUID, err)
			}
		}
	}
	for _, d := range ps.ghosts(c.underService) {
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
		if dev, ok := ps.devices[id]; ok && b.URI != "" && sameURL(dev.URI, b.URI) {
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
	if b.DeviceID == "" || b.URI == "" {
		return nil
	}
	id := strings.ToUpper(b.DeviceID)
	dev, ok := ps.devices[id]
	if !ok || !c.atURI(id, dev.URI, b.URI) {
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

// deviceDead is PMS's status for a device whose refresh failed.
const deviceDead = "dead"

// reviveEvery is how often a device that stays dead is asked about again.
const reviveEvery = time.Minute

// converge brings one block's device, DVR, channel map and guide into PMS.
func (c *Converger) converge(ctx context.Context, p Provider, b Block, ps *pass) error {
	id := strings.ToUpper(b.DeviceID)
	uri := b.URI
	if uri == "" {
		return fmt.Errorf("block %s has no URI yet: clustarr publishes status.blocks[].uri", b.Start)
	}
	lease, err := c.acquire(ctx, p, b, ps)
	if err != nil || !lease.Held {
		return err
	}

	if _, known := c.registered[id]; !known {
		var n registeredNote
		if json.Unmarshal([]byte(lease.Annotations[AnnotationRegistered]), &n) == nil && n.URI != "" {
			c.registered[id] = n
		}
	}
	dev, ok := ps.devices[id]
	if ok && !c.atURI(id, dev.URI, uri) {
		// The block's own device at a stale address (the link-local proxies'
		// before 2026-10-27, a renamed release): its DVRs and the device go,
		// then the block is registered at its URI. Registering beside it is
		// how duplicate rows arise.
		held := c.held[id]
		if err := c.remove(ctx, p, dev, ps); err != nil {
			return c.wait(p, b, "replace device "+dev.UUID+" at "+dev.URI, err)
		}
		c.held[id] = held
		delete(c.registered, id)
		c.note(ctx, p, id, "", savedHashes{}) // the old DVR's saves are no new DVR's
		c.record(p, ReasonReplaced, fmt.Sprintf("block %s: device %s was at %s; registering it at %s", b.Start, id, dev.URI, uri), false)
		ok = false
	}
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
		if !sameURL(dev.URI, uri) {
			c.recordOnce("stored/"+id, p, ReasonStoredElsewhere,
				fmt.Sprintf("block %s: PMS lists device %s at %s, not %s as registered; it is kept as PMS stored it", b.Start, id, dev.URI, uri), true)
		}
		c.noteRegistered(ctx, p, id, registeredNote{URI: uri, Stored: dev.URI})
	}

	// A device PMS marked dead is asked about again: PMS never retries
	// one itself, and shows its DVR as "Device not found". On 2026-11-04
	// PMS refreshed the device seven seconds after the manager's tuner
	// began listening, before its first render served the device's path,
	// and Live TV stayed off until a discover by hand.
	if dev.Status == deviceDead && c.now().Sub(c.revived[id]) >= reviveEvery {
		if err := c.PMS.DiscoverDevice(ctx, uri); err != nil {
			return fmt.Errorf("discover dead device %s again: %w", uri, err)
		}
		c.revived[id] = c.now()
		c.record(p, ReasonRevived, fmt.Sprintf("block %s: PMS had marked device %s dead; asked it to discover %s again", b.Start, dev.UUID, uri), true)
	}

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
		if err := c.idle(ctx, ps); err != nil {
			return c.wait(p, b, "create the DVR", err)
		}
		lang := c.Language
		if lang == "" {
			lang = "eng"
		}
		lineup := b.Lineup
		if lineup == "" {
			lineup = plexapi.XMLTVLineup(uri+b.GuidePath, b.FriendlyName)
		}
		created, err := c.PMS.CreateDVR(ctx, lang, dev.UUID, lineup)
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
		// A Plex block's map is the tuner's pairing (plexmap.json); an XMLTV
		// block's PMS pairs itself, each number with its own guide channel.
		var m []plexapi.ChannelMapping
		if b.Lineup != "" {
			m, err = c.plexMap(ctx, uri)
		} else {
			m, err = c.PMS.EPGChannelMap(ctx, dev.UUID, dvr.Lineup)
		}
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
	switch {
	case b.Lineup != "":
		// Plex's own guide: nothing of clustarr's to reload. PMS's nightly
		// refresh (the RefreshEpgGuides butler task, which cluster-plex keeps
		// on for Live TV; incremental since the shim stopped timing its
		// whole-guide read out, 2026-11-04) extends it; the lease holder
		// reloads it whole only once it has run short.
		if err := c.refreshCloudGuide(ctx, p, b, id, dvr, ps); err != nil {
			return err
		}
		if err := c.restoreMap(ctx, p, b, id, dev, dvr, uri, ps); err != nil {
			return err
		}
	case b.GuideHash != "" && s.guide != b.GuideHash:
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
	case c.emptyGuide(ctx, id, dvr):
		if err := c.idle(ctx, ps); err != nil {
			return c.wait(p, b, "reload the empty guide", err)
		}
		if err := c.PMS.ReloadGuide(ctx, string(dvr.Key)); err != nil {
			return fmt.Errorf("reload the empty guide: %w", err)
		}
		ps.guideBusy = true
		c.emptyReloaded[id] = c.now()
		c.log("Live TV: reloaded a guide that held no airings", "block", b.Start, "dvr", dvr.Key)
	}
	delete(c.told, "wait/"+id)
	return nil
}

// restoreMap saves a Plex block's channel map again when PMS has replaced
// it with a pairing of its own, which it does at the end of a guide load
// that failed (2026-11-04: every cloud load failed on the shim's FTS
// block, and PMS put NBC on CNBC 355). The map is read back from the
// device (plexapi.Client.Device) when the DVR's refreshedAt has moved since
// the last look, else every mapCheckEvery, never while PMS loads a guide;
// a channel the tuner pairs that PMS maps elsewhere, or not at all, or a
// channel PMS enabled that the tuner does not pair, is a replaced map. A
// save starts a whole load, so one is made at most every mapRestoreEvery
// per device, and a map still replaced within that is recorded once.
func (c *Converger) restoreMap(ctx context.Context, p Provider, b Block, id string, dev plexapi.Device, dvr plexapi.DVR, uri string, ps *pass) error {
	if ps.guideBusy || dvr.Key == "" {
		return nil
	}
	last, seen := c.mapChecked[id]
	if seen && last.refreshedAt == dvr.RefreshedAt && c.now().Sub(last.at) < mapCheckEvery {
		return nil
	}
	if err := c.idle(ctx, ps); err != nil {
		if errors.Is(err, errWaiting) {
			return nil // read back once the load ends
		}
		return err
	}
	have, err := c.PMS.Device(ctx, string(dev.Key))
	if err != nil {
		return fmt.Errorf("read device %s's channel map: %w", dev.UUID, err)
	}
	want, err := c.plexMap(ctx, uri)
	if err != nil {
		return fmt.Errorf("read the tuner's channel map: %w", err)
	}
	c.mapChecked[id] = mapCheck{at: c.now(), refreshedAt: dvr.RefreshedAt}
	missing, changed, extra := mapDifference(want, have.ChannelMappings)
	if missing+changed+extra == 0 {
		delete(c.told, "restore/"+id)
		return nil
	}
	what := fmt.Sprintf("block %s: PMS's map of device %s differs from the tuner's pairing: %d channels paired elsewhere, %d missing, %d PMS added",
		b.Start, dev.UUID, changed, missing, extra)
	if at, ok := c.mapRestored[id]; ok && c.now().Sub(at) < mapRestoreEvery {
		c.recordOnce("restore/"+id, p, ReasonChannelMapRestored, what+"; saved again within the hour, so not again yet", true)
		return nil
	}
	if n := plexapi.MapBytes(want); n > plexapi.MaxChannelMapBytes {
		return nil // reported by the save path
	}
	if err := c.PMS.SaveChannelMap(ctx, string(dev.Key), want); err != nil {
		if errors.Is(err, plexapi.ErrEmptyChannelMap) {
			return nil
		}
		return fmt.Errorf("save the channel map again (%d channels): %w", len(want), err)
	}
	ps.guideBusy = true // the save rebuilds the guide
	c.mapRestored[id] = c.now()
	delete(c.told, "restore/"+id)
	c.record(p, ReasonChannelMapRestored, what+"; saved the tuner's map again", true)
	return nil
}

// mapDifference compares the tuner's pairing want with the map PMS holds,
// have: channels want pairs that have lacks (missing) or pairs with another
// lineup channel (changed), and channels have enables that want does not
// pair (extra). A disabled entry of have is no pairing.
func mapDifference(want, have []plexapi.ChannelMapping) (missing, changed, extra int) {
	got := make(map[string]string, len(have))
	for _, m := range have {
		if m.Enabled != "" && m.Enabled != "1" {
			continue
		}
		got[m.DeviceIdentifier] = m.LineupIdentifier
	}
	wanted := make(map[string]bool, len(want))
	for _, m := range want {
		wanted[m.DeviceIdentifier] = true
		lineup, ok := got[m.DeviceIdentifier]
		switch {
		case !ok:
			missing++
		case lineup != m.LineupIdentifier:
			changed++
		}
	}
	for device := range got {
		if !wanted[device] {
			extra++
		}
	}
	return missing, changed, extra
}

// refreshCloudGuide reloads a Plex cloud lineup's guide whole once it runs
// short: in the CloudRefresh window, when the guide's horizon
// (GuideHorizon) is under cloudHorizonFloor away, so PMS's own nightly
// refresh has not extended it for a week. Never while PMS loads a guide,
// nor while a recording or a Live TV session runs (an ActivityGrab), which
// waits for the next pass and is recorded once. A whole load follows, as a
// saved map's does (about 30 min for 233 channels), and the ask is
// remembered for emptyGuideRetry, since the horizon moves only when the
// load ends. PMS moves the DVR's refreshedAt at the end of any refresh, a
// cancelled or failed one too (2026-11-04: a cancel mid-chunk left 165
// channels without two days of airings and refreshedAt moved anyway), so
// it says a refresh ended, not that the guide is whole; the horizon is
// the measure.
func (c *Converger) refreshCloudGuide(ctx context.Context, p Provider, b Block, id string, dvr plexapi.DVR, ps *pass) error {
	now := c.now().UTC()
	if c.GuideHorizon == nil || dvr.UUID == "" || !inWindow(now.Hour(), c.CloudRefresh) {
		return nil
	}
	if at, ok := c.cloudReloaded[id]; ok && now.Sub(at) < emptyGuideRetry {
		return nil
	}
	horizon, err := c.GuideHorizon(ctx, dvr.UUID)
	if err != nil {
		c.log("Live TV: could not read how far a guide reaches", "dvr", dvr.Key, "error", err)
		return nil
	}
	if horizon.Sub(now) >= cloudHorizonFloor {
		return nil
	}
	if err := c.idle(ctx, ps); err != nil {
		return c.wait(p, b, "reload the guide that runs short", err)
	}
	grabbing, err := c.grabbing(ctx)
	if err != nil {
		return err
	}
	if grabbing {
		c.recordOnce("refresh/"+id, p, ReasonRefreshWaiting,
			fmt.Sprintf("block %s: the guide runs short; its reload waits for a recording or Live TV session to end", b.Start), false)
		return nil
	}
	if err := c.PMS.ReloadGuide(ctx, string(dvr.Key)); err != nil {
		return fmt.Errorf("reload the guide that runs short: %w", err)
	}
	ps.guideBusy = true
	c.cloudReloaded[id] = now
	delete(c.told, "refresh/"+id)
	reach := "no airings"
	if !horizon.IsZero() {
		reach = "reaches " + horizon.UTC().Format("2006-01-02 15:04") + "Z"
	}
	c.record(p, ReasonGuideRefreshed,
		fmt.Sprintf("block %s: the guide of DVR %s %s; reloading it whole, since PMS's nightly refresh has not extended it", b.Start, dvr.Key, reach), false)
	return nil
}

// inWindow is whether hour h (0-23) lies in the window [start, end) of
// w, which may wrap midnight (23-2 is 23, 0 and 1); an empty window
// (start == end) holds no hour.
func inWindow(h int, w [2]int) bool {
	start, end := w[0], w[1]
	switch {
	case start == end:
		return false
	case start < end:
		return h >= start && h < end
	default:
		return h >= start || h < end
	}
}

// grabbing is whether PMS is recording or serving a Live TV session. It
// reads this pod's PMS alone, which is every PMS there is: Live TV holds the
// deployment to one Plex pod (cmd/manager's liveTVNeedsOnePod). Several pods
// would each need reading here, and before every delete, save and reload
// that waits on a guide load.
func (c *Converger) grabbing(ctx context.Context) (bool, error) {
	acts, err := c.PMS.Activities(ctx)
	if err != nil {
		return false, fmt.Errorf("list activities: %w", err)
	}
	for _, a := range acts {
		if a.Type == plexapi.ActivityGrab {
			return true, nil
		}
	}
	return false, nil
}

// maxPlexMapBytes caps the tuner's plexmap.json: a block's map is under
// 32 KiB of query, its JSON a few times that.
const maxPlexMapBytes = 1 << 20

// plexMap reads a Plex block's channel map from the tuner, at its URI.
func (c *Converger) plexMap(ctx context.Context, uri string) ([]plexapi.ChannelMapping, error) {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri+"/plexmap.json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s/plexmap.json: %w", uri, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s/plexmap.json: status %d", uri, resp.StatusCode)
	}
	var m []plexapi.ChannelMapping
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPlexMapBytes)).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode %s/plexmap.json: %w", uri, err)
	}
	return m, nil
}

// emptyGuide is whether a DVR's guide holds no airings and was not reloaded
// for being empty within the last emptyGuideRetry.
func (c *Converger) emptyGuide(ctx context.Context, id string, dvr plexapi.DVR) bool {
	if c.GuideEmpty == nil || dvr.UUID == "" {
		return false
	}
	if at, ok := c.emptyReloaded[id]; ok && c.now().Sub(at) < emptyGuideRetry {
		return false
	}
	empty, err := c.GuideEmpty(ctx, dvr.UUID)
	if err != nil {
		c.log("Live TV: could not tell whether a guide is empty", "dvr", dvr.Key, "error", err)
		return false
	}
	return empty
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

// atURI is whether a device PMS lists at devURI is block id's at uri: at
// uri itself, or where PMS stored it when this installation registered it
// there (AnnotationRegistered).
func (c *Converger) atURI(id, devURI, uri string) bool {
	if sameURL(devURI, uri) {
		return true
	}
	n, ok := c.registered[id]
	return ok && n.URI == uri && sameURL(n.Stored, devURI)
}

// noteRegistered records where block id's device was registered and where
// PMS lists it, in memory and on the block's Lease, so neither this
// converger nor the next holder replaces it as stale.
func (c *Converger) noteRegistered(ctx context.Context, p Provider, id string, n registeredNote) {
	c.registered[id] = n
	v, _ := json.Marshal(n)
	if err := c.Leases.Annotate(ctx, p.Namespace, LeaseName(id), c.Identity, map[string]string{AnnotationRegistered: string(v)}); err != nil {
		c.log("note the registered device on the block's Lease", "lease", LeaseName(id), "error", err)
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
	delete(c.registered, id)
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

// ghosts are DVRs whose XMLTV guide is under a livetv Service's /hdhr/ but
// which list no device PMS has: a row PMS kept after its device went
// (recorded).
func (ps *pass) ghosts(managed func(uri string) bool) []plexapi.DVR {
	var out []plexapi.DVR
	for _, d := range ps.dvrs {
		if !managed(lineupGuide(d.Lineup)) {
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

// lineupGuide is an XMLTV lineup id's guide URL; "" for any other lineup.
func lineupGuide(lineup string) string {
	rest, ok := strings.CutPrefix(lineup, "lineup://tv.plex.providers.epg.xmltv/")
	if !ok {
		return ""
	}
	if dec, err := url.QueryUnescape(rest); err == nil {
		rest = dec
	}
	rest, _, _ = strings.Cut(rest, "#")
	return rest
}

// underService is whether uri is a device or guide under the /hdhr/ of a
// livetv Service a live provider has named: clustarr's, wherever the device
// id is.
func (c *Converger) underService(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil || !strings.HasPrefix(u.Path, "/hdhr/") {
		return false
	}
	for a := range c.addresses {
		if sameHost(u, a) {
			return true
		}
	}
	return false
}

// deviceID is an HDHomeRun device's id, from its identifier; "" for any
// other kind of device.
func deviceID(d plexapi.Device) string {
	if id, ok := strings.CutPrefix(d.UUID, deviceIDPrefix); ok && id != "" {
		return strings.ToUpper(id)
	}
	return ""
}

// sameURL compares two device URIs as PMS may store them: scheme, host
// (case-insensitive), port with 80 or 443 implied by the scheme, and path
// with a trailing slash trimmed.
func sameURL(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return false
	}
	return ua.Scheme == ub.Scheme && strings.EqualFold(ua.Hostname(), ub.Hostname()) && portOf(ua) == portOf(ub) &&
		strings.TrimSuffix(ua.Path, "/") == strings.TrimSuffix(ub.Path, "/")
}

// sameHost is whether u is at address, a host:port as status.address names
// it, the port implied by u's scheme when u names none.
func sameHost(u *url.URL, address string) bool {
	a, err := url.Parse("http://" + address)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), a.Hostname()) && portOf(u) == portOf(a)
}

// portOf is u's port, else its scheme's.
func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
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
