package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/mediactl/clusterplex/pkg/lease"
	"github.com/mediactl/clusterplex/pkg/plexroute"
)

// Pod role labels, kept for observability only. Nothing routes on them: the
// Lease is the single source of truth and the proxy follows it directly.
const (
	RoleStarting = "starting"
	RoleLeader   = "leader"
	RoleWorker   = "worker"
)

const (
	// renewInterval is how often the holder extends its lease. It has to be
	// comfortably shorter than the lease duration.
	renewInterval = 5 * time.Second
	// acquireInterval is how often a pod that is not the holder tries again.
	acquireInterval = 2 * time.Second
	// pmsStartTimeout bounds how long we wait for Plex to bind its port before
	// giving up on advertising this pod.
	pmsStartTimeout = 5 * time.Minute
	// healthInterval is how often Plex is asked whether it is still serving,
	// once it has started. Frequent enough that a stalled Plex leaves the load
	// balancer quickly, rare enough to be free.
	healthInterval = 10 * time.Second
)

// runPlex starts Plex on this pod and keeps it running.
//
// With the library in PostgreSQL every pod reads and writes the same database,
// so there is no primary to elect and nothing to replicate. What still needs
// one owner is the connection to plex.tv, because every pod shares one server
// identity. That is what the lease decides.
//
// Pods that do not hold the lease have their route to plex.tv filtered, so
// running Plex on all of them no longer means several connections under one
// identity. The default is still one pod, because that path has been exercised
// and active mode has not.
func (m *Manager) runPlex(ctx context.Context) {
	if err := m.updatePodRole(ctx, RoleStarting); err != nil {
		m.Logger.Error("update pod role label", "error", err)
	}

	elector := &lease.Elector{
		Client:    m.K8sClient,
		Namespace: m.Config.Namespace,
		Name:      m.Config.LeaseName,
		Identity:  m.Config.PodName,
	}
	m.elector = elector

	if m.Config.PlexMode == plexModeActive {
		// Every pod serves. The lease still elects the plex.tv owner, which
		// the egress rules will follow once they exist.
		m.markReady(ctx, RoleWorker)
		m.applyEgress(ctx, false)
		m.start(ctx)
		go m.holdPlexTVLease(ctx, elector)
		return
	}
	go m.runElected(ctx, elector)
}

// runElected keeps trying to become the holder, and runs Plex only while it is.
func (m *Manager) runElected(ctx context.Context, elector *lease.Elector) {
	for {
		switch err := elector.Acquire(ctx); {
		case errors.Is(err, lease.ErrHeldByAnother):
			m.markReady(ctx, RoleWorker)
			m.applyEgress(ctx, false)
		case err != nil:
			m.Logger.Error("acquire lease", "error", err)
		default:
			m.Logger.Info("became the active Plex", "lease", m.Config.LeaseName)
			m.markReady(ctx, RoleLeader)
			m.Metrics.LeaderStatus.Set(1)
			m.applyEgress(ctx, true)
			m.start(ctx)
			m.holdUntilLost(ctx, elector)
			// Losing the lease means another pod is taking over. Exit so the
			// container comes back clean rather than half torn down.
			m.Logger.Warn("lost the lease; restarting as a standby")
			m.shutdown(ctx)
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(acquireInterval):
		}
	}
}

// applyEgress opens or closes this pod's route to Plex's own services.
//
// The lease decides who may hold the connection to plex.tv: every pod shares
// one server identity, and several pods holding it at once makes that identity
// appear to move between addresses. A pod that is not the holder can still
// serve media and reach metadata providers; it just cannot reach plex.tv.
func (m *Manager) applyEgress(ctx context.Context, holdsLease bool) {
	if m.egress == nil {
		return
	}
	if err := m.egress.Apply(ctx, holdsLease); err != nil {
		m.Logger.Error("apply egress policy", "holds_lease", holdsLease, "error", err)
	}
}

// holdUntilLost renews the lease until it is lost or the context ends.
func (m *Manager) holdUntilLost(ctx context.Context, elector *lease.Elector) {
	ticker := time.NewTicker(renewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := elector.Renew(ctx); err != nil {
			m.Logger.Warn("lease renewal failed", "error", err)
			return
		}
	}
}

// holdPlexTVLease competes for the plex.tv owner lease without gating Plex on
// it, which is what active mode needs.
func (m *Manager) holdPlexTVLease(ctx context.Context, elector *lease.Elector) {
	ticker := time.NewTicker(acquireInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := elector.Acquire(ctx); err == nil {
			m.Metrics.LeaderStatus.Set(1)
			m.applyEgress(ctx, true)
			m.holdUntilLost(ctx, elector)
			m.Metrics.LeaderStatus.Set(0)
			m.applyEgress(ctx, false)
		}
	}
}

// setRunsPlex records that this pod serves Plex, so its readiness answers for
// Plex rather than for the manager alone.
func (m *Manager) setRunsPlex(runs bool) {
	m.mu.Lock()
	m.runsPlex = runs
	m.mu.Unlock()
}

// start brings up Plex and advertises this pod once it is accepting.
func (m *Manager) start(ctx context.Context) {
	// Before the start, not after: from here on this pod's readiness answers
	// for Plex, and a failed start must not leave it claiming otherwise.
	m.setRunsPlex(true)
	if err := m.sup.Start(ctx); err != nil {
		m.Logger.Error("start Plex Media Server", "error", err)
		return
	}
	go m.advertiseWhenAccepting(ctx)
}

func (m *Manager) markReady(ctx context.Context, role string) {
	m.mu.Lock()
	already := !m.isStarting
	m.isStarting, m.isReady = false, true
	m.mu.Unlock()
	if already {
		return
	}
	if err := m.updatePodRole(ctx, role); err != nil {
		m.Logger.Error("update pod role label", "error", err)
	}
}

// advertiseWhenAccepting publishes this pod as routable only once Plex is
// answering requests, not merely once its port is open.
//
// Plex binds the port within a second and then answers 503 to everything until
// it has finished starting, so accepting a connection proves nothing. Worse, it
// can abort part way through and stay exactly there: port open, every request
// 503, indefinitely. Advertising on the port alone sends clients to a server
// that will never answer them.
func (m *Manager) advertiseWhenAccepting(ctx context.Context) {
	if err := plexroute.WaitServing(ctx, m.plexURL(), pmsStartTimeout); err != nil {
		m.Logger.Error("Plex never started serving; not advertising this pod", "error", err)
		return
	}
	manager := net.JoinHostPort(m.Config.PodDNS(), strconv.Itoa(m.Config.ProbePort))
	if err := m.publisher.Publish(ctx, m.Config.PMSAddr(), "http://"+manager); err != nil {
		m.Logger.Error("advertise Plex availability", "error", err)
		return
	}
	// Also record it on the pod itself. The maintenance fan-out needs the set
	// of pods that can do work, which the Lease cannot express: it names one
	// holder, and in active mode every pod is serving.
	if err := m.markServingPlex(ctx, true); err != nil {
		m.Logger.Error("mark this pod as serving Plex", "error", err)
	}
	m.Logger.Info("advertised this pod as a routable Plex", "address", m.Config.PMSAddr())
	go m.watchPlexHealth(ctx)
}

// plexURL reaches Plex inside its own network namespace. It must not go
// through the pod's own port: the proxy holds that and answers whether Plex is
// up or not, so a health check aimed there grades the proxy.
func (m *Manager) plexURL() string { return "http://" + m.plexAddr }

// watchPlexHealth withdraws this pod when Plex stops answering.
//
// Starting is not the only time Plex can stop serving while holding its port,
// and the supervisor cannot see it: Plex is wrapped in a subreaper that stays
// alive as long as any descendant does, so a Plex that has aborted its main
// loop still looks like a running process. Without this the pod keeps its
// readiness and its serving annotation forever.
func (m *Manager) watchPlexHealth(ctx context.Context) {
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := plexroute.Serving(ctx, m.plexURL())
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			m.setPlexServing(ctx, true)
			continue
		}
		m.Logger.Error("Plex is no longer serving; withdrawing this pod", "error", err)
		m.setPlexServing(ctx, false)
	}
}

// setPlexServing records whether traffic should come here, in both the places
// that decide it: this pod's readiness, and the annotation the proxy and the
// maintenance fan-out read.
func (m *Manager) setPlexServing(ctx context.Context, serving bool) {
	m.mu.Lock()
	changed := m.plexServing != serving
	m.plexServing = serving
	m.mu.Unlock()
	if !changed {
		return
	}
	if err := m.markServingPlex(ctx, serving); err != nil {
		m.Logger.Error("update the serving annotation", "serving", serving, "error", err)
	}
}

// withdraw stops traffic being routed here before Plex goes away.
func (m *Manager) withdraw(ctx context.Context) {
	if m.publisher == nil {
		return
	}
	if err := m.publisher.Clear(ctx); err != nil {
		m.Logger.Error("withdraw Plex availability", "error", err)
	}
	if err := m.markServingPlex(ctx, false); err != nil {
		m.Logger.Error("mark this pod as no longer serving Plex", "error", err)
	}
}

// shutdown withdraws this pod, stops Plex and releases the lease.
func (m *Manager) shutdown(ctx context.Context) {
	m.withdraw(ctx)
	if err := m.sup.Stop(ctx); err != nil {
		m.Logger.Error("stop Plex Media Server", "error", err)
	}
	if m.elector != nil {
		if err := m.elector.Release(ctx); err != nil {
			m.Logger.Error("release lease", "error", err)
		}
	}
	m.Metrics.LeaderStatus.Set(0)
}

func (m *Manager) updatePodRole(ctx context.Context, role string) error {
	payload := []byte(fmt.Sprintf(`{"metadata":{"labels":{"plex-role":"%s"}}}`, role))
	_, err := m.patchPod(ctx, payload)
	return err
}
