package livetv_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	"github.com/mediactl/clusterplex/pkg/plex/livetv"
)

// fakePMS is PMS's Live TV API as recorded (docs/livetv-pms-calls.md), in
// memory: a device is registered from its URI, whose /hdhr/<id> path names
// the device id clustarr's tuner would answer in discover.json; a DVR lists its device, whose
// parentID becomes the DVR's key; creating a DVR, saving a channel map or
// reloading a guide starts a provider.epg.load that lasts until the test
// ends it; and a delete, save, reload or DVR creation sent during that load
// fails the test, as the delete that deadlocked the recorded PMS.
type fakePMS struct {
	t *testing.T

	mu       sync.Mutex
	next     int
	devices  []plexapi.Device
	dvrs     []plexapi.DVR
	loading  bool
	channels int            // the EPG channel map's size
	failNext map[string]int // "METHOD /path" -> status, once
	// writes is every request that is not a GET, as "METHOD path".
	writes []string
	// saves counts channel-map saves per device key.
	saves map[string]int
	// loadIn starts a load once that many more /activities reads have
	// answered idle: PMS beginning a guide refresh of its own mid-pass.
	loadIn int
	// storeAs, when set, is the URI PMS keeps for a device registered at
	// uri: a PMS that normalises what it was given.
	storeAs func(uri string) string
}

func newFakePMS(t *testing.T) (*fakePMS, *plexapi.Client) {
	f := &fakePMS{t: t, next: 1, channels: 3, failNext: map[string]int{}, saves: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, &plexapi.Client{BaseURL: srv.URL, Token: func() string { return "tok" }}
}

func (f *fakePMS) key() string {
	k := strconv.Itoa(f.next)
	f.next++
	return k
}

// addReal registers a device the converger must never touch: the owner's
// own HDHomeRun, with a DVR on it.
func (f *fakePMS) addReal() (plexapi.Device, plexapi.DVR) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := plexapi.Device{Key: plexapi.Str(f.key()), UUID: "device://tv.plex.grabbers.hdhomerun/1076C3A7", URI: "http://192.168.1.50", DeviceID: "1076C3A7", Model: "HDHomeRun FLEX QUATRO", State: "enabled"}
	f.devices = append(f.devices, d)
	v := plexapi.DVR{Key: plexapi.Str(f.key()), UUID: "real-dvr", Lineup: "lineup://tv.plex.providers.epg.onconnect/US-12345", Devices: []plexapi.Device{d}}
	f.dvrs = append(f.dvrs, v)
	return d, v
}

func (f *fakePMS) endLoad() {
	f.mu.Lock()
	f.loading = false
	f.mu.Unlock()
}

func (f *fakePMS) startLoad() {
	f.mu.Lock()
	f.loading = true
	f.mu.Unlock()
}

// startLoadAfter starts a load once n more /activities reads answered idle.
func (f *fakePMS) startLoadAfter(n int) {
	f.mu.Lock()
	f.loadIn = n
	f.mu.Unlock()
}

func (f *fakePMS) failOnce(method, path string, status int) {
	f.mu.Lock()
	f.failNext[method+" "+path] = status
	f.mu.Unlock()
}

func (f *fakePMS) takeWrites() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.writes
	f.writes = nil
	return w
}

func (f *fakePMS) state() ([]plexapi.Device, []plexapi.DVR) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.devices), slices.Clone(f.dvrs)
}

func (f *fakePMS) answer(w http.ResponseWriter, key string, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{key: v}})
}

func (f *fakePMS) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path, q := r.URL.Path, r.URL.Query()
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+path)
	}
	if code, ok := f.failNext[r.Method+" "+path]; ok {
		delete(f.failNext, r.Method+" "+path)
		w.WriteHeader(code)
		return
	}
	if f.loading && guideWrite(r.Method, path) {
		f.t.Errorf("DEADLOCK: %s %s while provider.epg.load runs", r.Method, path)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	switch {
	case r.Method == http.MethodGet && path == "/media/grabbers/devices":
		f.answer(w, "Device", f.devices)
	case r.Method == http.MethodPost && path == "/media/grabbers/devices":
		u, err := url.Parse(q.Get("uri"))
		id, ok := "", false
		if err == nil {
			id, ok = strings.CutPrefix(strings.TrimSuffix(u.Path, "/"), "/hdhr/")
		}
		if !ok || id == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		stored := q.Get("uri")
		if f.storeAs != nil {
			stored = f.storeAs(stored)
		}
		d := plexapi.Device{Key: plexapi.Str(f.key()), UUID: "device://tv.plex.grabbers.hdhomerun/" + id, URI: stored, DeviceID: id, Model: "clustarr", State: "enabled"}
		f.devices = append(f.devices, d)
		f.answer(w, "Device", []plexapi.Device{d})
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/media/grabbers/devices/"):
		k := strings.TrimPrefix(path, "/media/grabbers/devices/")
		n := len(f.devices)
		f.devices = slices.DeleteFunc(f.devices, func(d plexapi.Device) bool { return string(d.Key) == k })
		if len(f.devices) == n {
			w.WriteHeader(http.StatusNotFound)
		}
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/channelmap"):
		k := strings.TrimSuffix(strings.TrimPrefix(path, "/media/grabbers/devices/"), "/channelmap")
		f.saves[k]++
		f.loading = true // a save rebuilds the guide (recorded)
	case r.Method == http.MethodGet && path == "/livetv/dvrs":
		f.answer(w, "Dvr", f.dvrs)
	case r.Method == http.MethodPost && path == "/livetv/dvrs":
		var dev *plexapi.Device
		for i := range f.devices {
			if f.devices[i].UUID == q.Get("device") {
				dev = &f.devices[i]
			}
		}
		if dev == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		v := plexapi.DVR{Key: plexapi.Str(f.key()), UUID: fmt.Sprintf("dvr-%d", f.next), Language: q.Get("language"), Lineup: q.Get("lineup"), Devices: []plexapi.Device{*dev}}
		dev.ParentID, _ = strconv.Atoi(string(v.Key))
		f.dvrs = append(f.dvrs, v)
		f.loading = true
		f.answer(w, "Dvr", []plexapi.DVR{v})
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/livetv/dvrs/"):
		k := strings.TrimPrefix(path, "/livetv/dvrs/")
		n := len(f.dvrs)
		f.dvrs = slices.DeleteFunc(f.dvrs, func(d plexapi.DVR) bool { return string(d.Key) == k })
		if len(f.dvrs) == n {
			w.WriteHeader(http.StatusNotFound)
		}
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/reloadGuide"):
		f.loading = true
	case r.Method == http.MethodGet && path == "/livetv/epg/channelmap":
		m := make([]plexapi.ChannelMapping, f.channels)
		for i := range m {
			id := strconv.Itoa(1000 + i)
			m[i] = plexapi.ChannelMapping{ChannelKey: id, DeviceIdentifier: id, LineupIdentifier: id}
		}
		f.answer(w, "ChannelMapping", m)
	case r.Method == http.MethodGet && path == "/activities":
		var acts []plexapi.Activity
		if f.loading {
			acts = append(acts, plexapi.Activity{UUID: "a1", Type: plexapi.ActivityGuideLoad, Title: "Refreshing EPG"})
		} else if f.loadIn > 0 {
			if f.loadIn--; f.loadIn == 0 {
				f.loading = true
			}
		}
		f.answer(w, "Activity", acts)
	default:
		f.t.Errorf("fake PMS: unexpected %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// guideWrite is a request the recorded rule forbids during a guide load:
// deleting a DVR or device, saving a map, reloading a guide, or creating a
// DVR (which starts a second load).
func guideWrite(method, path string) bool {
	switch {
	case method == http.MethodDelete:
		return true
	case method == http.MethodPut && strings.HasSuffix(path, "/channelmap"):
		return true
	case method == http.MethodPost && (path == "/livetv/dvrs" || strings.HasSuffix(path, "/reloadGuide")):
		return true
	}
	return false
}

// fakeLeases is a LeaseClient: a Lease is held by whoever took it first,
// unless the test marks it expired.
type fakeLeases struct {
	mu      sync.Mutex
	holder  map[string]string
	expired map[string]bool
	labels  map[string]map[string]string
	// calls counts Acquire calls per Lease: each is a renewal when held.
	calls map[string]int
	notes map[string]map[string]string
}

func newFakeLeases() *fakeLeases {
	return &fakeLeases{holder: map[string]string{}, expired: map[string]bool{}, labels: map[string]map[string]string{}, calls: map[string]int{}, notes: map[string]map[string]string{}}
}

func (l *fakeLeases) acquired(k string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls[k]
}

func (l *fakeLeases) Acquire(_ context.Context, namespace, name, identity string, labels map[string]string) (livetv.Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := namespace + "/" + name
	l.calls[k]++
	if h := l.holder[k]; h != "" && h != identity && !l.expired[k] {
		return livetv.Lease{Holder: h}, nil
	}
	if l.holder[k] != identity {
		delete(l.notes, k)
	}
	l.holder[k], l.expired[k], l.labels[k] = identity, false, labels
	return livetv.Lease{Held: true, Holder: identity, Annotations: maps.Clone(l.notes[k])}, nil
}

func (l *fakeLeases) Annotate(_ context.Context, namespace, name, identity string, annotations map[string]string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := namespace + "/" + name
	if l.holder[k] != identity {
		return fmt.Errorf("lease %s is %s's", k, l.holder[k])
	}
	if l.notes[k] == nil {
		l.notes[k] = map[string]string{}
	}
	maps.Copy(l.notes[k], annotations)
	return nil
}
