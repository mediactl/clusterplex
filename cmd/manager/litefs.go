package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/mediactl/clusterplex/pkg/litefsk8s"
	"github.com/superfly/litefs"
	"github.com/superfly/litefs/fuse"
	"github.com/superfly/litefs/http"
)

func (s *Manager) startLiteFS(ctx context.Context) error {
	dataDir := "/var/lib/litefs"
	fuseDir := "/var/lib/plexmediaserver/Library/Application Support/Plex Media Server/Plug-in Support/Databases"

	// Ensure directories exist
	os.MkdirAll(dataDir, 0755)
	os.MkdirAll(fuseDir, 0755)

	store := litefs.NewStore(dataDir, true)
	
	advertiseURL := fmt.Sprintf("http://%s.plex-workers.%s.svc.cluster.local:20202", s.PodName, s.Namespace)
	store.Leaser = litefsk8s.NewK8sLeaser(s.K8sClient, s.Namespace, "cluster-plex-litefs", s.PodName, advertiseURL)

	if err := store.Open(); err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	fsys := fuse.NewFileSystem(fuseDir, store)
	fsys.AllowOther = true
	if err := fsys.Mount(false); err != nil {
		return fmt.Errorf("mount fuse: %w", err)
	}
	store.Invalidator = fsys

	// Start LiteFS HTTP server for replicas
	httpServer := http.NewServer(store, ":20202")
	if err := httpServer.Listen(); err != nil {
		return fmt.Errorf("listen http: %w", err)
	}
	go httpServer.Serve()

	// Monitor primary status and start/stop PMS
	go s.monitorPrimaryStatus(ctx, store)

	return nil
}

func (s *Manager) monitorPrimaryStatus(ctx context.Context, store *litefs.Store) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			isPrimary := store.IsPrimary()
			
			s.mu.Lock()
			if isPrimary && !s.isLeader {
				// Transition to Leader
				s.Logger.Info("LiteFS became primary. Starting Plex Media Server.")
				s.Metrics.LeaderStatus.Set(1)
				s.isLeader = true
				s.isStarting = false
				s.isReady = true
				
				s.pmsCmd = exec.CommandContext(ctx, "/usr/lib/plexmediaserver/Plex Media Server")
				s.pmsCmd.Stdout = os.Stdout
				s.pmsCmd.Stderr = os.Stderr
				s.pmsCmd.Start()
			} else if !isPrimary && s.isLeader {
				// Transition from Leader to Worker
				s.Logger.Warn("LiteFS lost primary status. Shutting down Plex Media Server.")
				s.Metrics.LeaderStatus.Set(0)
				s.isLeader = false
				if s.pmsCmd != nil && s.pmsCmd.Process != nil {
					s.pmsCmd.Process.Kill()
				}
				os.Exit(0) // Exit to restart pod safely
			} else if !isPrimary && s.isStarting {
			    // Mark as ready worker
			    s.Logger.Info("LiteFS node running as replica.")
			    s.isStarting = false
			    s.isReady = true
			}
			s.mu.Unlock()
		}
	}
}
