package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/superfly/litefs"
	"github.com/superfly/litefs/fuse"
	"github.com/superfly/litefs/http"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/mediactl/clusterplex/pkg/litefsk8s"
	"github.com/mediactl/clusterplex/pkg/plexroute"
)

// Pod role labels. The client-facing Service selects RoleLeader; the job
// dispatcher selects RoleWorker. A pod that has just started carries
// RoleStarting so a label left behind by a crashed leader does not linger.
// pmsStartTimeout bounds how long we wait for Plex to bind its port before
// giving up on advertising this pod.
const pmsStartTimeout = 5 * time.Minute

// clusterIDCheckInterval is how often a running node re-checks that its
// LiteFS lineage still matches the cluster's.
const clusterIDCheckInterval = 15 * time.Second

const (
	RoleStarting = "starting"
	RoleLeader   = "leader"
	RoleWorker   = "worker"
)

// startLiteFS opens the store, mounts FUSE over Plex's Databases directory,
// serves replication to other nodes, and starts watching for primary status.
func (m *Manager) startLiteFS(ctx context.Context) error {
	cfg := m.Config
	if err := m.updatePodRole(ctx, RoleStarting); err != nil {
		m.Logger.Error("update pod role label", "error", err)
	}
	fuseDir := cfg.DatabasesDir()
	if err := os.MkdirAll(cfg.LiteFSDir, 0o755); err != nil {
		return fmt.Errorf("create litefs dir: %w", err)
	}
	if err := os.MkdirAll(fuseDir, 0o755); err != nil {
		return fmt.Errorf("create databases dir: %w", err)
	}

	leaser := litefsk8s.NewK8sLeaser(m.K8sClient, cfg.Namespace, cfg.LeaseName, cfg.PodName, cfg.AdvertiseURL())

	if err := m.reconcileClusterID(ctx, leaser); err != nil {
		m.Logger.Error("check LiteFS cluster id", "error", err)
	}

	store := litefs.NewStore(cfg.LiteFSDir, true)
	store.Leaser = leaser
	store.Client = http.NewClient()
	if err := store.Open(); err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	fsys := fuse.NewFileSystem(fuseDir, store)
	fsys.AllowOther = true
	if err := fsys.Mount(false); err != nil {
		return fmt.Errorf("mount fuse: %w", err)
	}
	store.Invalidator = fsys

	server := http.NewServer(store, fmt.Sprintf(":%d", cfg.LiteFSPort))
	if err := server.Listen(); err != nil {
		return fmt.Errorf("listen litefs http: %w", err)
	}
	go server.Serve()

	m.store, m.fsys, m.litefsHTTP = store, fsys, server
	go m.monitorPrimaryStatus(ctx, store)
	go m.monitorClusterID(ctx, store, leaser)
	return nil
}

// advertiseWhenAccepting publishes this pod as the routable Plex only once its
// port actually accepts a connection. Holding the Lease means this pod should
// run Plex; it says nothing about whether Plex has finished starting, and
// advertising too early sends clients to a closed port.
func (m *Manager) advertiseWhenAccepting(ctx context.Context) {
	local := net.JoinHostPort("127.0.0.1", strconv.Itoa(m.Config.PMSPort))
	if err := plexroute.WaitListening(ctx, local, pmsStartTimeout); err != nil {
		m.Logger.Error("Plex never started accepting connections; not advertising this pod", "error", err)
		return
	}
	manager := net.JoinHostPort(m.Config.PodDNS(), strconv.Itoa(m.Config.ProbePort))
	if err := m.publisher.Publish(ctx, m.Config.PMSAddr(), "http://"+manager); err != nil {
		m.Logger.Error("advertise Plex availability", "error", err)
		return
	}
	m.Logger.Info("advertised this pod as the routable Plex", "address", m.Config.PMSAddr())
}

// withdraw stops traffic being routed here before Plex goes away.
func (m *Manager) withdraw(ctx context.Context) {
	if m.publisher == nil {
		return
	}
	if err := m.publisher.Clear(ctx); err != nil {
		m.Logger.Error("withdraw Plex availability", "error", err)
	}
}

// reconcileClusterID compares this node's LiteFS lineage with the cluster's.
//
// LiteFS refuses to replicate between nodes whose cluster IDs differ, and
// nothing reconciles it, so a node that disagrees silently serves whatever it
// last had. It is not safe to resolve automatically: whichever pod wins the
// election first stamps its ID on the Lease, so an empty node can make the
// node holding the real library look like the outlier. Adopting the cluster's
// lineage discards this node's data, so it happens only when asked. Otherwise
// the node is marked orphaned, which keeps it out of every Service and out of
// the election until an operator decides.
func (m *Manager) reconcileClusterID(ctx context.Context, leaser *litefsk8s.K8sLeaser) error {
	established, err := leaser.ClusterID(ctx)
	if err != nil {
		return err
	}
	local, err := litefsk8s.LocalClusterID(m.Config.LiteFSDir)
	if err != nil {
		return err
	}
	if established == "" || local == "" || established == local {
		return nil
	}

	if m.Config.AdoptClusterID {
		m.Logger.Warn("discarding this node's LiteFS lineage and resnapshotting from the primary",
			"local", local, "cluster", established)
		return litefsk8s.AdoptClusterID(m.Config.LiteFSDir)
	}

	m.Logger.Error("node is orphaned and will not be marked ready", "local", local, "cluster", established)
	m.setOrphaned(orphanReason(local, established))
	return nil
}

// orphanReason explains the state and the remedy in one line, because it
// surfaces through the readiness probe.
func orphanReason(local, established string) string {
	return fmt.Sprintf("this node's LiteFS lineage %s does not match the cluster's %s, so it replicates nothing; "+
		"confirm which lineage holds the real library, then restart the orphaned nodes with --litefs-adopt-cluster-id",
		local, established)
}

// monitorClusterID keeps watching for lineage divergence after startup. The
// Lease's cluster ID can be established or corrected while this node is
// already running, and a node that only checked once would keep reporting
// Ready while replicating nothing.
func (m *Manager) monitorClusterID(ctx context.Context, store *litefs.Store, leaser *litefsk8s.K8sLeaser) {
	ticker := time.NewTicker(clusterIDCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		established, err := leaser.ClusterID(ctx)
		if err != nil {
			m.Logger.Warn("check cluster id", "error", err)
			continue
		}
		local := store.ClusterID()
		if established == "" || local == "" || established == local {
			continue
		}

		m.mu.RLock()
		already := m.orphaned != ""
		m.mu.RUnlock()
		if already {
			continue
		}
		m.Logger.Error("node became orphaned: its LiteFS lineage no longer matches the cluster",
			"local", local, "cluster", established)
		m.setOrphaned(orphanReason(local, established))
	}
}

// monitorPrimaryStatus starts Plex when this node becomes primary and exits
// the process when it stops being primary, so the pod restarts as a clean
// replica.
func (m *Manager) monitorPrimaryStatus(ctx context.Context, store *litefs.Store) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		isPrimary := store.IsPrimary()

		m.mu.Lock()
		switch {
		case isPrimary && !m.isLeader:
			m.Logger.Info("LiteFS became primary; starting Plex Media Server")
			if err := m.updatePodRole(ctx, RoleLeader); err != nil {
				m.Logger.Error("update pod role label", "error", err)
			}
			m.Metrics.LeaderStatus.Set(1)
			m.isLeader, m.isStarting, m.isReady = true, false, true
			if err := m.sup.Start(ctx); err != nil {
				m.Logger.Error("start Plex Media Server", "error", err)
			} else {
				go m.advertiseWhenAccepting(ctx)
			}
		case !isPrimary && m.isLeader:
			m.Logger.Warn("LiteFS lost primary status; stopping Plex Media Server and restarting as a replica")
			m.Metrics.LeaderStatus.Set(0)
			m.isLeader, m.isReady = false, false
			m.mu.Unlock()
			m.restartAsReplica(ctx)
			return
		case !isPrimary && m.isStarting:
			m.Logger.Info("LiteFS node running as replica")
			if err := m.updatePodRole(ctx, RoleWorker); err != nil {
				m.Logger.Error("update pod role label", "error", err)
			}
			m.isStarting, m.isReady = false, true
		}
		m.mu.Unlock()
	}
}

// restartAsReplica stops Plex and exits so the container comes back as a
// clean replica; LiteFS's in-process state does not survive a demotion.
func (m *Manager) restartAsReplica(ctx context.Context) {
	m.withdraw(ctx)
	if err := m.sup.Stop(ctx); err != nil {
		m.Logger.Error("stop Plex Media Server", "error", err)
	}
	m.shutdownLiteFS()
	os.Exit(0)
}

// shutdownLiteFS releases the lease, stops replication and unmounts FUSE.
func (m *Manager) shutdownLiteFS() {
	if m.litefsHTTP != nil {
		_ = m.litefsHTTP.Close()
	}
	if m.store != nil {
		if err := m.store.Close(); err != nil {
			m.Logger.Error("close litefs store", "error", err)
		}
	}
	if m.fsys != nil {
		if err := m.fsys.Unmount(); err != nil {
			m.Logger.Error("unmount fuse", "error", err)
		}
	}
}

func (m *Manager) updatePodRole(ctx context.Context, role string) error {
	payload := []byte(fmt.Sprintf(`{"metadata":{"labels":{"plex-role":"%s"}}}`, role))
	_, err := m.K8sClient.CoreV1().Pods(m.Config.Namespace).Patch(ctx, m.Config.PodName, types.StrategicMergePatchType, payload, metav1.PatchOptions{})
	return err
}
