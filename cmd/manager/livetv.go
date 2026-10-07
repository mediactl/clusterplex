package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	plexlivetv "github.com/mediactl/clusterplex/pkg/plex/livetv"
)

// liveTVTimeout bounds one Live TV call: a channel map of thousands of
// channels takes longer than the client's 30 s default.
const liveTVTimeout = 2 * time.Minute

// liveTVLoop is the lease holder's Live TV work: converge Plex's DVRs on
// every change the pod's watch (startLiveTV) reports and every
// provisionEvery, recording Events on the providers.
func (m *Manager) liveTVLoop(ctx context.Context) {
	log := m.Logger.With("component", "livetv")
	if m.liveTV == nil {
		return
	}
	broadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: m.K8sClient.EventsV1()})
	broadcaster.StartRecordingToSink(ctx.Done())
	defer broadcaster.Shutdown()
	recorder := broadcaster.NewRecorder(scheme.Scheme, "cluster-plex")

	every := m.provisionEvery
	if every <= 0 {
		every = defaultProvisionEvery
	}
	conv := &plexlivetv.Converger{
		PMS:          &plexapi.Client{BaseURL: "http://" + m.plexAddr, Token: m.plexToken, HTTP: &http.Client{Timeout: liveTVTimeout}},
		Language:     m.Config.LiveTV.Language,
		LocalAddress: m.Config.LiveTV.LocalAddress,
		// A pass renews the Leases; three periods ride out a slow PMS.
		Leases: &plexlivetv.Leases{Client: m.K8sClient, Duration: max(plexlivetv.DefaultLeaseDuration, 3*every)},
		PMSStarted: func() time.Time {
			m.mu.RLock()
			defer m.mu.RUnlock()
			return m.pmsStarted
		},
		Logger: log,
		Record: func(e plexlivetv.Event) { recordLiveTVEvent(recorder, e) },
	}
	changes, unsubscribe := m.liveTV.subscribe()
	defer unsubscribe()
	restarts := newLiveTVRestarts()
	go m.runLiveTVRestarts(ctx, restarts, m.Config.LiveTV.RestartEvery, conv.Record, log)

	backoff := min(provisionBackoffStart, every)
	var (
		live   []plexlivetv.Provider
		synced bool
	)
	timer := time.NewTimer(every)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case live = <-changes:
			synced = true
		case <-timer.C:
		}
		wait := every
		if synced && conv.Identity == "" {
			// The Leases' holder: the server's machineIdentifier.
			id, err := conv.PMS.Identity(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				log.Warn("Live TV waits for the server's identity", "error", err, "in", backoff)
				m.Metrics.ProvisionRuns.WithLabelValues("livetv-" + string(plexlivetv.Errored)).Inc()
				wait, backoff = backoff, min(backoff*2, every)
			} else {
				conv.Identity = id
			}
		}
		if synced && conv.Identity != "" {
			res, err := conv.Run(ctx, live)
			if ctx.Err() != nil {
				return
			}
			m.Metrics.ProvisionRuns.WithLabelValues("livetv-" + string(res.Status)).Inc()
			if res.DVRChanged {
				restarts.request(res.Changed)
			}
			if res.RestartSelf {
				if err := m.restartOwnPMS(ctx); err != nil {
					log.Warn("Live TV: ask this pod's PMS to restart so it lists every DVR", "error", err)
				}
			}
			if res.Status == plexlivetv.Pending || res.Status == plexlivetv.Errored {
				log.Warn("Live TV did not converge; retrying", "error", err, "in", backoff)
			}
			wait, backoff = nextLiveTVPass(res.Status, backoff, every)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
	}
}

// liveTVGuidePoll is how soon a pass that only waited for a guide load runs
// again.
var liveTVGuidePoll = 20 * time.Second

// nextLiveTVPass is how long the loop waits after a pass of status, and the
// backoff after that.
func nextLiveTVPass(status plexlivetv.Status, backoff, every time.Duration) (wait, next time.Duration) {
	switch status {
	case plexlivetv.Converged:
		return every, min(provisionBackoffStart, every)
	case plexlivetv.Waiting:
		// A guide load ends on its own, in minutes; backing off to
		// provisionEvery made a 32-block provider take hours.
		return min(liveTVGuidePoll, every), min(provisionBackoffStart, every)
	}
	return backoff, min(backoff*2, every)
}

// liveTVSyncWait bounds how long PMS waits at start for the first list of
// IPTVProviders, so the blocks' proxies are up before PMS asks its devices.
const liveTVSyncWait = 30 * time.Second

// liveTVRetry is how often a proxy that could not bind is tried again.
const liveTVRetry = 30 * time.Second

// startLiveTV runs on every pod, leader or not, before PMS starts (Live TV
// spec §5.0): it adds the link-local address to lo, watches clustarr's
// IPTVProviders, and serves every block's port through m.proxies. It waits
// for the first list up to liveTVSyncWait; past that PMS starts anyway and
// the proxies follow. The lease holder's liveTVLoop reads the same feed.
func (m *Manager) startLiveTV(ctx context.Context) {
	log := m.Logger.With("component", "livetv")
	addr := m.Config.LiveTV.LocalAddress
	if err := plexlivetv.EnsureLinkLocal(addr); err != nil {
		log.Error("add the Live TV address; PMS cannot reach the tuner's blocks", "address", addr, "error", err)
	}
	m.proxies = &plexlivetv.ProxySet{LocalAddress: addr, Logger: log}
	m.liveTV = &liveTVFeed{}
	first, unsubscribe := m.liveTV.subscribe()

	w := &plexlivetv.Watcher{
		Dynamic: m.Dynamic, Discovery: m.K8sClient.Discovery(), Namespace: m.Config.Clustarr.Namespace, Logger: log,
		OnChange: m.liveTV.set,
	}
	go func() {
		if err := w.Run(ctx); err != nil {
			log.Error("the IPTVProvider watch ended", "error", err)
		}
	}()
	go m.serveLiveTVProxies(ctx, log)

	defer unsubscribe()
	select {
	case <-first:
	case <-time.After(liveTVSyncWait):
		log.Warn("no list of IPTVProviders yet; starting Plex, the blocks' proxies follow", "waited", liveTVSyncWait)
	case <-ctx.Done():
	}
}

// serveLiveTVProxies applies every change to the proxies, retrying a port
// that could not be bound.
func (m *Manager) serveLiveTVProxies(ctx context.Context, log *slog.Logger) {
	changes, unsubscribe := m.liveTV.subscribe()
	defer unsubscribe()
	defer m.proxies.Close()
	var (
		live  []plexlivetv.Provider
		retry <-chan time.Time
	)
	for {
		select {
		case <-ctx.Done():
			return
		case live = <-changes:
		case <-retry:
		}
		retry = nil
		if err := m.proxies.Apply(live); err != nil {
			log.Warn("a block's proxy could not listen; retrying", "error", err, "in", liveTVRetry)
			retry = time.After(liveTVRetry)
		}
	}
}

// liveTVFeed hands the latest IPTVProviders to every subscriber: the
// proxies on every pod, and the lease holder's loop. A slow subscriber sees
// only the latest list.
type liveTVFeed struct {
	mu     sync.Mutex
	live   []plexlivetv.Provider
	synced bool
	subs   map[chan []plexlivetv.Provider]struct{}
}

func (f *liveTVFeed) set(live []plexlivetv.Provider) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live, f.synced = live, true
	for ch := range f.subs {
		offer(ch, live)
	}
}

// subscribe returns a channel that receives the current list, once there is
// one, and every later one.
func (f *liveTVFeed) subscribe() (<-chan []plexlivetv.Provider, func()) {
	ch := make(chan []plexlivetv.Provider, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = map[chan []plexlivetv.Provider]struct{}{}
	}
	f.subs[ch] = struct{}{}
	if f.synced {
		offer(ch, f.live)
	}
	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.subs, ch)
	}
}

// offer replaces whatever ch holds with live.
func offer(ch chan []plexlivetv.Provider, live []plexlivetv.Provider) {
	select {
	case <-ch:
	default:
	}
	ch <- live
}

// liveTVActions names what the lease holder did for each reason, the
// events.k8s.io Event's action.
var liveTVActions = map[string]string{
	plexlivetv.ReasonProvisioned:      "Provision",
	plexlivetv.ReasonChannelMapSaved:  "SaveChannelMap",
	plexlivetv.ReasonNeedsLineup:      "AwaitWizard",
	plexlivetv.ReasonFailed:           "Provision",
	plexlivetv.ReasonRemoved:          "Delete",
	plexlivetv.ReasonBlockOwned:       "AcquireLease",
	plexlivetv.ReasonDeviceIDTaken:    "CompareAddress",
	plexlivetv.ReasonWaitingForGuide:  "AwaitGuideLoad",
	plexlivetv.ReasonChannelMapTooBig: "SaveChannelMap",
	plexlivetv.ReasonPodsRestarted:    "RestartPMS",
	plexlivetv.ReasonDVRListStale:     "RestartPMS",
}

// recordLiveTVEvent records e on its IPTVProvider through client-go's
// events.k8s.io recorder, which aggregates repeats into a series.
func recordLiveTVEvent(recorder events.EventRecorder, e plexlivetv.Event) {
	ref := &corev1.ObjectReference{
		APIVersion: plexlivetv.GVR.GroupVersion().String(), Kind: "IPTVProvider",
		Namespace: e.Namespace, Name: e.Name, UID: types.UID(e.UID),
	}
	kind := corev1.EventTypeNormal
	if e.Warning {
		kind = corev1.EventTypeWarning
	}
	action := liveTVActions[e.Reason]
	if action == "" {
		action = "Provision"
	}
	recorder.Eventf(ref, nil, kind, e.Reason, action, "%s", e.Message)
}
