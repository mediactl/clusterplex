package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/superfly/litefs"
	"github.com/superfly/litefs/fuse"
	"github.com/superfly/litefs/http"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/mediactl/clusterplex/pkg/litefsk8s"
)

// Pod role labels. The client-facing Service selects RoleLeader; the job
// dispatcher selects RoleWorker. A pod that has just started carries
// RoleStarting so a label left behind by a crashed leader does not linger.
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

	store := litefs.NewStore(cfg.LiteFSDir, true)
	store.Leaser = litefsk8s.NewK8sLeaser(m.K8sClient, cfg.Namespace, cfg.LeaseName, cfg.PodName, cfg.AdvertiseURL())
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
	return nil
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
