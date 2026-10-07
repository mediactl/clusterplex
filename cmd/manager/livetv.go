package main

import (
	"context"
	"net/http"
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

// liveTVLoop is the lease holder's Live TV work: watch clustarr's
// IPTVProviders and converge Plex's DVRs on every change and every
// provisionEvery, recording Events on the providers.
func (m *Manager) liveTVLoop(ctx context.Context) {
	log := m.Logger.With("component", "livetv")
	ns := m.Config.Clustarr.Namespace
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
		Logger: log,
		Record: func(e plexlivetv.Event) { recordLiveTVEvent(recorder, e) },
	}
	changes := make(chan []plexlivetv.Provider, 1)
	w := &plexlivetv.Watcher{
		Dynamic: m.Dynamic, Discovery: m.K8sClient.Discovery(), Namespace: ns, Logger: log,
		OnChange: func(live []plexlivetv.Provider) {
			// Latest wins.
			for {
				select {
				case <-changes:
				default:
				}
				select {
				case changes <- live:
					return
				default:
				}
			}
		},
	}
	go func() {
		if err := w.Run(ctx); err != nil {
			log.Error("the IPTVProvider watch ended", "error", err)
		}
	}()

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
			switch res.Status {
			case plexlivetv.Converged:
				backoff = min(provisionBackoffStart, every)
			default:
				log.Warn("Live TV did not converge; retrying", "error", err, "in", backoff)
				wait, backoff = backoff, min(backoff*2, every)
			}
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
