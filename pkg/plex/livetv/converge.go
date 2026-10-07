// Package livetv adds each clustarr IPTVProvider to Plex as a DVR (clustarr's
// Live TV spec 2026-10-07 §6.1): the lease holder registers the provider's
// HDHomeRun device, creates its DVR over the provider's XMLTV guide, saves
// the full channel map through the API -- which carries more than the
// wizard's 480 channels -- and reloads the guide when it changes. It
// manages only devices whose identifier carries a provider's device id
// (rule M), and deletes only those: a deleted provider's DVR and device,
// duplicate DVR rows, and orphans its lineup URL names.
package livetv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

// deviceIDPrefix is an HDHomeRun device's identifier before its device id.
const deviceIDPrefix = "device://tv.plex.grabbers.hdhomerun/"

// The Event reasons recorded on an IPTVProvider.
const (
	ReasonProvisioned     = "DVRProvisioned"
	ReasonChannelMapSaved = "DVRChannelMapSaved"
	ReasonNeedsLineup     = "DVRNeedsLineup"
	ReasonAddressChanged  = "DVRDeviceAddressChanged"
	ReasonFailed          = "DVRProvisionFailed"
	ReasonRemoved         = "DVRRemoved"
)

// Result is how far a pass got.
type Result string

const (
	Converged Result = "converged"
	Pending   Result = "pending"
	Errored   Result = "error"
)

// Provider is an IPTVProvider as the converger needs it.
type Provider struct {
	Namespace, Name, UID string
	DeviceID, Address    string
	GuideURL             string
	FriendlyName         string
	EPGSource            string // "XEPG" | "PMS"
	LineupHash           string
	GuideHash            string
	Ready, Enabled       bool
}

func (p Provider) key() string { return p.Namespace + "/" + p.Name }

// Event is one thing a pass did or failed to do, for a provider.
type Event struct {
	Namespace, Name, UID string
	Reason, Message      string
	Warning              bool
}

// Converger runs passes. It is used from one goroutine at a time.
type Converger struct {
	PMS *plexapi.Client
	// Namespace is clustarr's: a DVR whose lineup URL is under
	// /livetv/<Namespace>/ belongs to one of its providers.
	Namespace string
	// Language is the DVR's guide language. Default "eng".
	Language string
	Logger   *slog.Logger
	Record   func(Event)

	mu sync.Mutex
	// saved is, by device id, the lineup and guide hashes last sent to PMS.
	// In memory only: a restart saves the map and reloads the guide once.
	saved map[string]savedHashes
	// told dedupes the warnings a pass would otherwise repeat every time.
	told map[string]string
}

type savedHashes struct{ lineup, guide string }

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

// Run is one idempotent pass: converge every live provider that is Ready
// and enabled, remove what gone providers and orphans left in PMS. A
// provider's failure is recorded and makes the pass Pending; the others
// still converge.
func (c *Converger) Run(ctx context.Context, live, gone []Provider) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.saved == nil {
		c.saved, c.told = map[string]savedHashes{}, map[string]string{}
	}
	devices, err := c.PMS.Devices(ctx)
	if err != nil {
		return Errored, fmt.Errorf("list devices: %w", err)
	}
	dvrs, err := c.PMS.DVRs(ctx)
	if err != nil {
		return Errored, fmt.Errorf("list DVRs: %w", err)
	}
	st := newState(devices, dvrs)

	live = slices.Clone(live)
	slices.SortFunc(live, func(a, b Provider) int { return strings.Compare(a.key(), b.key()) })
	liveIDs, liveNames := map[string]bool{}, map[string]bool{}
	pending := false
	var errs []error
	for _, p := range live {
		liveNames[p.key()] = true
		if p.DeviceID == "" {
			continue
		}
		liveIDs[p.DeviceID] = true
		if !p.Ready || !p.Enabled || p.Address == "" {
			continue
		}
		if err := c.converge(ctx, p, st); err != nil {
			c.record(p, ReasonFailed, err.Error(), true)
			pending = true
			errs = append(errs, fmt.Errorf("%s: %w", p.key(), err))
		}
	}
	for _, p := range gone {
		if p.DeviceID == "" || liveIDs[p.DeviceID] {
			continue
		}
		if err := c.remove(ctx, p, p.DeviceID, st); err != nil {
			pending = true
			errs = append(errs, fmt.Errorf("%s: %w", p.key(), err))
		}
	}
	for _, o := range st.orphans(c.Namespace, liveNames, liveIDs) {
		if err := c.remove(ctx, o, o.DeviceID, st); err != nil {
			pending = true
			errs = append(errs, fmt.Errorf("orphan %s: %w", o.key(), err))
		}
	}
	switch {
	case len(errs) > 0 && !pending:
		return Errored, errors.Join(errs...)
	case pending:
		return Pending, errors.Join(errs...)
	}
	return Converged, nil
}

func (c *Converger) converge(ctx context.Context, p Provider, st *state) error {
	uri := "http://" + p.Address
	dev, ok := st.devices[p.DeviceID]
	if !ok {
		if _, err := c.PMS.DiscoverDevice(ctx, uri); err != nil {
			return fmt.Errorf("discover %s: %w", uri, err)
		}
		if err := c.PMS.AddDevice(ctx, uri); err != nil && !errors.Is(err, plexapi.ErrConflict) {
			return fmt.Errorf("add device %s: %w", uri, err)
		}
		devices, err := c.PMS.Devices(ctx)
		if err != nil {
			return fmt.Errorf("list devices: %w", err)
		}
		st.addDevices(devices)
		if dev, ok = st.devices[p.DeviceID]; !ok {
			return fmt.Errorf("PMS lists no device %s after adding %s", p.DeviceID, uri)
		}
		delete(c.saved, p.DeviceID)
		c.record(p, ReasonProvisioned, "registered device "+dev.UUID+" at "+uri, false)
	} else if h := hostOf(dev.URI); h != p.Address {
		// Never register again: that is how duplicate rows arise. The
		// Service is never recreated, so a person changed something.
		if c.told["addr/"+p.DeviceID] != dev.URI {
			c.told["addr/"+p.DeviceID] = dev.URI
			c.record(p, ReasonAddressChanged, fmt.Sprintf("PMS has device %s at %s, the provider is at %s; changing nothing", dev.UUID, dev.URI, uri), true)
		}
		return nil
	}
	delete(c.told, "addr/"+p.DeviceID)

	dvrs := st.dvrsOf(dev.UUID)
	for _, extra := range dvrs[min(1, len(dvrs)):] {
		if err := c.PMS.DeleteDVR(ctx, string(extra.Key)); err != nil {
			return fmt.Errorf("delete duplicate DVR %s: %w", extra.Key, err)
		}
		c.record(p, ReasonRemoved, "deleted duplicate DVR "+string(extra.Key), false)
	}
	var dvr plexapi.DVR
	if len(dvrs) > 0 {
		dvr = dvrs[0]
	} else {
		if p.EPGSource == "PMS" {
			if c.told["lineup/"+p.DeviceID] == "" {
				c.told["lineup/"+p.DeviceID] = "told"
				c.record(p, ReasonNeedsLineup, "epgSource is PMS: finish the DVR once in Plex's setup wizard, choosing your location's lineup; the channel map is then kept for you", false)
			}
			return nil
		}
		lang := c.Language
		if lang == "" {
			lang = "eng"
		}
		created, err := c.PMS.CreateDVR(ctx, lang, dev.UUID, plexapi.XMLTVLineup(p.GuideURL, p.FriendlyName))
		if err != nil {
			return fmt.Errorf("create DVR: %w", err)
		}
		if created.Key == "" {
			all, err := c.PMS.DVRs(ctx)
			if err != nil {
				return fmt.Errorf("list DVRs: %w", err)
			}
			st.setDVRs(all)
			got := st.dvrsOf(dev.UUID)
			if len(got) == 0 {
				return errors.New("PMS lists no DVR on the device after creating one")
			}
			created = got[0]
		}
		dvr = created
		delete(c.saved, p.DeviceID)
		c.record(p, ReasonProvisioned, "created DVR "+string(dvr.Key), false)
	}
	delete(c.told, "lineup/"+p.DeviceID)

	s := c.saved[p.DeviceID]
	if s.lineup != p.LineupHash {
		m, err := c.PMS.EPGChannelMap(ctx, dev.UUID, dvr.Lineup)
		if err != nil {
			return fmt.Errorf("read the EPG channel map: %w", err)
		}
		if err := c.PMS.SaveChannelMap(ctx, string(dev.Key), m); err != nil {
			return fmt.Errorf("save the channel map (%d channels): %w", len(m), err)
		}
		s.lineup = p.LineupHash
		c.saved[p.DeviceID] = s
		c.record(p, ReasonChannelMapSaved, fmt.Sprintf("saved the channel map: %d channels", len(m)), false)
	}
	if p.GuideHash != "" && s.guide != p.GuideHash {
		if err := c.PMS.ReloadGuide(ctx, string(dvr.Key)); err != nil {
			return fmt.Errorf("reload the guide: %w", err)
		}
		s.guide = p.GuideHash
		c.saved[p.DeviceID] = s
	}
	return nil
}

// remove deletes a managed device's DVRs, then the device.
func (c *Converger) remove(ctx context.Context, p Provider, deviceID string, st *state) error {
	dev, ok := st.devices[deviceID]
	if !ok {
		delete(c.saved, deviceID)
		return nil
	}
	for _, d := range st.dvrsOf(dev.UUID) {
		if err := c.PMS.DeleteDVR(ctx, string(d.Key)); err != nil {
			return fmt.Errorf("delete DVR %s: %w", d.Key, err)
		}
	}
	if err := c.PMS.DeleteDevice(ctx, string(dev.Key)); err != nil {
		return fmt.Errorf("delete device %s: %w", dev.Key, err)
	}
	delete(st.devices, deviceID)
	delete(c.saved, deviceID)
	c.record(p, ReasonRemoved, "deleted the DVR and device "+dev.UUID, false)
	return nil
}

// state is PMS's devices and DVRs for one pass.
type state struct {
	devices map[string]plexapi.Device // by device id, HDHomeRun only
	dvrs    []plexapi.DVR
}

func newState(devices []plexapi.Device, dvrs []plexapi.DVR) *state {
	st := &state{devices: map[string]plexapi.Device{}}
	st.addDevices(devices)
	st.setDVRs(dvrs)
	return st
}

func (st *state) addDevices(devices []plexapi.Device) {
	for _, d := range devices {
		if id := deviceID(d); id != "" {
			st.devices[id] = d
		}
	}
}

func (st *state) setDVRs(dvrs []plexapi.DVR) { st.dvrs = dvrs }

// dvrsOf is the DVRs over device uuid, lowest key first.
func (st *state) dvrsOf(uuid string) []plexapi.DVR {
	var out []plexapi.DVR
	for _, d := range st.dvrs {
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

// orphans are the DVRs on managed devices that a deleted provider left:
// their XMLTV lineup URL names a provider of namespace that no longer
// exists. A PMS-mode provider's DVR has no such URL and cannot be found.
func (st *state) orphans(namespace string, liveNames, liveIDs map[string]bool) []Provider {
	if namespace == "" {
		return nil
	}
	pat := regexp.MustCompile(`^/livetv/` + regexp.QuoteMeta(namespace) + `/([^/]+)/xmltv\.xml$`)
	var out []Provider
	seen := map[string]bool{}
	for _, d := range st.dvrs {
		name, ok := lineupProvider(d.Lineup, pat)
		if !ok || liveNames[namespace+"/"+name] {
			continue
		}
		for _, dev := range d.Devices {
			id := deviceID(dev)
			if id == "" || liveIDs[id] || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, Provider{Namespace: namespace, Name: name, DeviceID: id})
		}
	}
	return out
}

// lineupProvider reads the provider name out of an XMLTV lineup id whose
// guide URL matches pat.
func lineupProvider(lineup string, pat *regexp.Regexp) (string, bool) {
	rest, ok := strings.CutPrefix(lineup, "lineup://tv.plex.providers.epg.xmltv/")
	if !ok {
		return "", false
	}
	if dec, err := url.QueryUnescape(rest); err == nil {
		rest = dec
	}
	rest, _, _ = strings.Cut(rest, "#")
	u, err := url.Parse(rest)
	if err != nil {
		return "", false
	}
	m := pat.FindStringSubmatch(u.Path)
	if m == nil {
		return "", false
	}
	return m[1], true
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

func compareKeys(a, b string) int {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		return ai - bi
	}
	return strings.Compare(a, b)
}
