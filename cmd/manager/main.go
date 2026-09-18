// The manager runs in every Plex pod. It embeds LiteFS to replicate Plex's
// SQLite databases, elects one pod to run Plex Media Server, fronts that
// server with a TCP proxy, and turns the other pods into workers that run
// transcodes the leader hands them.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"github.com/superfly/litefs"
	"github.com/superfly/litefs/fuse"
	litefshttp "github.com/superfly/litefs/http"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/mediactl/clusterplex/pkg/plexnet"
	"github.com/mediactl/clusterplex/pkg/plexprefs"
	"github.com/mediactl/clusterplex/pkg/plexroute"
	"github.com/mediactl/clusterplex/pkg/proxy"
	"github.com/mediactl/clusterplex/pkg/telemetry"
)

// Manager holds the pod-wide state and dependencies.
type Manager struct {
	Config    Config
	Logger    *slog.Logger
	Tracer    trace.Tracer
	Metrics   *telemetry.Metrics
	K8sClient kubernetes.Interface

	sup       *Supervisor
	publisher *plexroute.Publisher
	// plexAddr reaches Plex inside its network namespace, bypassing the proxy.
	// Anything asking "is Plex up?" has to use this: in the pod namespace the
	// proxy holds Plex's port, and it answers whether Plex is running or not.
	plexAddr     string
	store        *litefs.Store
	fsys         *fuse.FileSystem
	litefsHTTP   *litefshttp.Server
	shimServer   *grpc.Server
	workerServer *grpc.Server

	mu         sync.RWMutex
	isLeader   bool
	isReady    bool
	isStarting bool
	// orphaned, when set, is why this node's data is not replicating. It keeps
	// the pod out of every Service and out of the election.
	orphaned string
}

// setOrphaned marks this node as holding data that no longer replicates.
func (m *Manager) setOrphaned(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.orphaned = reason
}

func main() {
	os.Exit(run())
}

// run wires the manager and blocks until SIGTERM; it returns the exit code.
func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return 0
		}
		logger.Error("invalid configuration", "error", err)
		return 2
	}
	tracer, metrics := telemetry.InitTelemetry()

	k8sClient, err := newK8sClient()
	if err != nil {
		logger.Error("load kubernetes config", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	m := &Manager{
		Config:     cfg,
		Logger:     logger,
		Tracer:     tracer,
		Metrics:    metrics,
		K8sClient:  k8sClient,
		isStarting: true,
	}
	m.publisher = &plexroute.Publisher{
		Client: k8sClient, Namespace: cfg.Namespace, LeaseName: cfg.LeaseName, Pod: cfg.PodName,
	}

	// Plex gets its own network namespace so it can keep binding 32400 and
	// 32401 without taking them from the pod, where the proxy now wants 32400.
	// This is provisioned on every pod rather than on election: a veth pair
	// costs nothing, and a failure here is much easier to act on at boot than
	// during a failover.
	plexNet, err := plexnet.Provision(ctx, plexnet.Config{
		Subnet:   cfg.PlexSubnet,
		PlexPort: cfg.PMSPort,
	}, logger.With("component", "plexnet"))
	if err != nil {
		// Fatal, unlike the port redirect it replaces. Starting Plex without
		// its namespace would have it bind the proxy's port and die with the
		// reason recorded only in its own log.
		logger.Error("provision Plex network namespace", "error", err)
		return 1
	}
	defer func() {
		if err := plexNet.Close(); err != nil {
			logger.Error("tear down Plex network namespace", "error", err)
		}
	}()
	// Everything that needs to reach Plex directly, rather than through the
	// proxy, uses this address.
	m.plexAddr = plexNet.PlexAddrPort().String()

	m.sup = &Supervisor{
		Binary:       cfg.PMSBinary,
		PIDFile:      cfg.PIDFile(),
		Logger:       logger.With("component", "supervisor"),
		Grace:        defaultGrace,
		StartProcess: plexNet.StartProcess,
		Preferences: func(context.Context) error {
			changed, err := plexprefs.Apply(cfg.PreferencesFile(), cfg.Preferences)
			if err != nil {
				return err
			}
			if len(changed) > 0 {
				logger.Info("applied Plex preferences", "file", cfg.PreferencesFile(), "changed", changed)
			}
			return nil
		},
		Proxy: &proxy.TCP{
			// The proxy takes Plex's own port in the pod namespace. Anything
			// reaching the pod for 32400 — the Service, a worker's progress
			// callback, a client following Plex's advertisement — lands here,
			// with no redirect rule to install and nothing to bypass.
			Listen:       fmt.Sprintf(":%d", cfg.PMSPort),
			Target:       plexNet.PlexAddrPort().String(),
			Logger:       logger.With("component", "proxy"),
			OnConnChange: func(delta int) { metrics.ProxyConnections.Add(float64(delta)) },
		},
		OnUnexpectedExit: func(err error) {
			// LiteFS state lives in this process; the cleanest recovery is a
			// fresh container, which re-runs the election.
			logger.Error("exiting so the pod restarts and re-elects", "error", err)
			m.shutdownLiteFS()
			os.Exit(1)
		},
	}

	go m.serveProbes()
	if err := m.startExecServers(ctx); err != nil {
		logger.Error("start job listeners", "error", err)
		return 1
	}
	if err := m.startLiteFS(ctx); err != nil {
		logger.Error("start LiteFS", "error", err)
		return 1
	}

	<-ctx.Done()
	logger.Info("shutting down")
	m.withdraw(context.Background())
	stopCtx, cancel := context.WithTimeout(context.Background(), defaultGrace+5*time.Second)
	defer cancel()
	if err := m.sup.Stop(stopCtx); err != nil {
		logger.Error("stop Plex Media Server", "error", err)
	}
	m.shutdownLiteFS()
	return 0
}

func (m *Manager) serveProbes() {
	addr := fmt.Sprintf(":%d", m.Config.ProbePort)
	m.Logger.Info("probe and metrics server listening", "addr", addr)
	srv := &http.Server{Addr: addr, Handler: m.probeHandler(), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		m.Logger.Error("probe server stopped", "error", err)
	}
}

func newK8sClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = os.ExpandEnv("$HOME/.kube/config")
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
	}
	return kubernetes.NewForConfig(cfg)
}
