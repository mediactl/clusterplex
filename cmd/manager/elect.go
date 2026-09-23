package main

import (
	"context"
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
	// unhealthyRestartAfter is how many checks in a row Plex may miss before
	// the pod is given up on and restarted. One miss is a slow request or a
	// restarting proxy; three in a row is Plex gone.
	unhealthyRestartAfter = 3
)

// plexHealth folds the health checks together and decides when this pod is
// past saving.
type plexHealth struct {
	consecutiveFailures int
}

// record takes one check's result and reports whether Plex is serving, and
// whether recovering it should be attempted now.
//
// giveUp is true once per run of failures rather than once per tick, so the
// recovery is not started again while it is still happening — and the count
// starts over when it fires, so a Plex that does not come back is tried again
// rather than left alone. That second part matters: while losing Plex meant
// exiting the pod, firing once ever was right; now that it means restarting
// Plex, a restart that does not take has to be noticed.
func (h *plexHealth) record(err error) (serving, giveUp bool) {
	if err == nil {
		h.consecutiveFailures = 0
		return true, false
	}
	h.consecutiveFailures++
	if h.consecutiveFailures < unhealthyRestartAfter {
		return false, false
	}
	h.consecutiveFailures = 0
	return false, true
}

// runPlex starts Plex on this pod and keeps it running.
//
// Every pod runs Plex. With the library in PostgreSQL they all read and write
// the same database, so there is no primary to elect and nothing to replicate,
// and a pod that is up either serves or is broken — there is no third state
// where it is running, not the leader, and not ready.
//
// What still needs exactly one owner is the connection to plex.tv, because
// every pod runs under one server identity and several publishing at once
// makes that identity appear to move between addresses. That, and only that,
// is what the lease decides: the holder's route to plex.tv is open and
// everyone else's is dropped. See ADR-0004.
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

	// Closed before Plex starts, not after: a pod must never reach plex.tv
	// while it is still deciding whether it owns it.
	m.releasePlexTV(ctx)
	m.start(ctx)
	go m.holdPlexTVLease(ctx, elector)
}

// takePlexTV records that this pod now owns the connection to plex.tv and
// opens its route there.
//
// It deliberately does not touch Plex. Winning the lease does not make this
// pod serve, because it was already serving.
func (m *Manager) takePlexTV(ctx context.Context) {
	m.Logger.Info("this pod now owns plex.tv", "lease", m.Config.LeaseName)
	m.Metrics.LeaderStatus.Set(1)
	m.markReady(ctx, RoleLeader)
	m.applyEgress(ctx, true)
}

// releasePlexTV gives it up again and drops the route.
//
// It deliberately does not touch Plex either, and that is the whole point of
// ADR-0004. Stopping Plex here used to leave the pod unable to recover: it
// stopped re-acquiring the lease and never reset its readiness, so it sat at
// 0/1 for ever and the rolling update that was waiting on it never finished.
func (m *Manager) releasePlexTV(ctx context.Context) {
	m.Metrics.LeaderStatus.Set(0)
	m.markReady(ctx, RoleWorker)
	m.applyEgress(ctx, false)
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
			m.takePlexTV(ctx)
			m.holdUntilLost(ctx, elector)
			m.Logger.Warn("gave up plex.tv to another pod; still serving",
				"lease", m.Config.LeaseName)
			m.releasePlexTV(ctx)
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
	// The databases have to be ready before Plex opens them, and the shadow is
	// rebuilt on every start rather than kept, because the shim writes DDL to
	// it as it runs and a kept one drifts from what PostgreSQL holds.
	if err := m.prepareDatabases(ctx); err != nil {
		// Fatal: Plex against a half-prepared database fails much later and
		// much less clearly, part way through its own migrations.
		m.Logger.Error("prepare the library and shadow databases", "error", err)
		return
	}
	if err := m.sup.Start(ctx); err != nil {
		m.Logger.Error("start Plex Media Server", "error", err)
		return
	}
	go m.advertiseWhenAccepting(ctx)
}

// markReady marks this pod ready and records the role it is ready in.
//
// The role is written whenever it changes, not only on the first call. It used
// to be first-call-only, which was harmless while the first call was the one
// that knew this pod's role — and stopped being harmless when taking and
// giving up the lease became ordinary transitions a running pod makes.
func (m *Manager) markReady(ctx context.Context, role string) {
	m.mu.Lock()
	changed := m.isStarting || m.role != role
	m.isStarting, m.isReady, m.role = false, true, role
	m.mu.Unlock()
	if !changed {
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
	m.startHealthWatch(ctx, healthInterval)
}

// startHealthWatch begins watching Plex for this stretch of leadership.
//
// Scoped rather than tied to the pod's own context, because losing the lease
// is not losing Plex: the pod stops Plex itself and carries on as a standby.
// A watch that outlived that found Plex gone and restarted the container.
func (m *Manager) startHealthWatch(ctx context.Context, interval time.Duration) {
	watchCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	previous := m.stopHealth
	m.stopHealth = cancel
	m.mu.Unlock()
	if previous != nil {
		previous()
	}
	go m.watchPlexHealth(watchCtx, interval)
}

// stopHealthWatch ends it again, before Plex is stopped on purpose.
func (m *Manager) stopHealthWatch() {
	m.mu.Lock()
	cancel := m.stopHealth
	m.stopHealth = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// plexURL reaches Plex inside its own network namespace. It must not go
// through the pod's own port: the proxy holds that and answers whether Plex is
// up or not, so a health check aimed there grades the proxy.
func (m *Manager) plexURL() string { return "http://" + m.plexAddr }

// watchPlexHealth withdraws this pod when Plex stops answering, and restarts
// the pod when it stays that way.
//
// Starting is not the only time Plex can stop serving while holding its port,
// and the supervisor cannot see it: Plex is wrapped in a subreaper that stays
// alive as long as any descendant does, so a Plex that has aborted its main
// loop still looks like a running process — its plug-ins outlive it, cmd.Wait
// never returns and OnUnexpectedExit never fires. This is the only thing that
// notices, so it has to be the thing that recovers too; withdrawing alone left
// the pod 0/1 for ever with a dead Plex inside it.
func (m *Manager) watchPlexHealth(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var health plexHealth
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
		serving, giveUp := health.record(err)
		m.setPlexServing(ctx, serving)
		if serving {
			continue
		}
		m.Logger.Error("Plex is no longer serving; withdrawing this pod", "error", err)
		if giveUp {
			m.Logger.Error("Plex has not answered since; giving up on this pod",
				"checks", unhealthyRestartAfter, "error", err)
			m.plexLost(err)
		}
	}
}

// plexLost hands this pod over to whatever restarts it.
func (m *Manager) plexLost(err error) {
	if m.onPlexLost == nil {
		return
	}
	m.onPlexLost(err)
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
//
// The health watch goes first. Everything below is a deliberate stop, and the
// watch cannot tell one of those from Plex dying on its own — left running it
// would count three missed checks and restart the container on the way out.
func (m *Manager) shutdown(ctx context.Context) {
	m.stopHealthWatch()
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
