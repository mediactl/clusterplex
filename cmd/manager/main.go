// The manager runs in every Plex pod. It gives Plex its own network namespace,
// supervises it, fronts it with a TCP proxy, answers the media proxy's
// questions about the library, and runs the helper binaries the shim
// intercepts.
//
// The library itself lives in PostgreSQL, shared by every pod, so there is no
// database to replicate and no primary to elect. The lease that remains elects
// the one pod allowed to hold the connection to plex.tv, because every pod
// shares a single server identity.
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
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/mediactl/clusterplex/pkg/lease"
	plexdb "github.com/mediactl/clusterplex/pkg/plex/db"
	plexnet "github.com/mediactl/clusterplex/pkg/plex/net"
	plexprefs "github.com/mediactl/clusterplex/pkg/plex/prefs"
	plexroute "github.com/mediactl/clusterplex/pkg/plex/route"
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
	DB        *plexdb.DB
	// pool is the same connection, unwrapped, for the bootstrap: it needs to
	// run statements and multi-row queries rather than single-row lookups.
	pool *plexdb.Pool

	sup       *Supervisor
	publisher *plexroute.Publisher
	elector   *lease.Elector
	egress    *plexnet.EgressGuard
	// plexAddr reaches Plex inside its network namespace, bypassing the proxy.
	// Anything asking "is Plex up?" has to use this: in the pod namespace the
	// proxy holds Plex's port, and it answers whether Plex is running or not.
	plexAddr     string
	shimServer   *grpc.Server
	workerServer *grpc.Server
	// onPlexLost gives this pod up. Both ways of losing Plex end here: the
	// supervisor seeing it exit, and the health watch seeing it stop
	// answering. Tests replace it; in the manager it restarts the container.
	onPlexLost func(err error)
	// stopHealth ends the health watch started for the current leadership.
	// A pod that loses the lease stops Plex on purpose, and the watch has to
	// go with it: left running it finds Plex gone and restarts the container
	// over what is really a clean handover. Guarded by mu.
	stopHealth context.CancelFunc

	mu         sync.RWMutex
	isReady    bool
	isStarting bool
	// role is what this pod's plex-role label last said, so a transition can
	// be told from a repeat. Guarded by mu.
	role string
	// runsPlex is whether this pod is one that serves Plex, as opposed to a
	// worker that only takes transcode jobs. A worker is ready as soon as it
	// has a role; a pod serving Plex is not ready until Plex answers.
	runsPlex bool
	// plexServing is whether Plex last answered a request. It starts false, so
	// winning the election is never on its own enough to take traffic.
	plexServing bool
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

	pool, err := plexdb.Open(ctx, cfg.Postgres)
	if err != nil {
		logger.Error("connect to the library database", "database", cfg.Postgres.String(), "error", err)
		return 1
	}
	defer pool.Close()
	logger.Info("library database configured", "database", cfg.Postgres.String())

	if err := checkShim(cfg); err != nil {
		// Without it Plex quietly uses its own SQLite file, which would look
		// like an empty library rather than a failure.
		logger.Error("the PostgreSQL shim is not available", "error", err)
		return 1
	}

	m := &Manager{
		Config:     cfg,
		Logger:     logger,
		Tracer:     tracer,
		Metrics:    metrics,
		K8sClient:  k8sClient,
		DB:         &plexdb.DB{Querier: pool},
		pool:       pool,
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
		// This is process start, so a veth already holding the name is one a
		// previous container in this same pod left behind: the pod's network
		// namespace outlives the container inside it. Without this the pod
		// crash-loops for ever after its first restart, whatever caused it.
		ReclaimStaleLink: true,
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

	// Only the lease holder may reach plex.tv. Every pod shares one server
	// identity, and several holding that connection at once makes the identity
	// appear to move between addresses, which breaks remote access.
	m.egress = &plexnet.EgressGuard{
		Blocklist: plexNet.Blocklist(),
		BlockAll:  cfg.BlockPlexTV,
		Logger:    logger.With("component", "egress"),
	}

	m.sup = &Supervisor{
		Binary:       cfg.PMSBinary,
		Subreaper:    cfg.SubreaperBinary,
		StateDir:     cfg.PlexDir,
		PIDFile:      cfg.PIDFile(),
		Env:          shimEnv(os.Environ(), cfg),
		Logger:       logger.With("component", "supervisor"),
		Grace:        defaultGrace,
		Drain:        cfg.DrainTimeout,
		StartProcess: plexNet.StartProcess,
		Preferences: func(ctx context.Context) error {
			// Resolved here rather than at load because the address is the
			// cluster's to assign: a LoadBalancer that had none when the
			// manager started has one by the next start.
			prefs := m.Config.EnforcedPreferences(m.externalURL(ctx))
			changed, err := plexprefs.Apply(cfg.PreferencesFile(), prefs)
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
		OnUnexpectedExit: func(err error) { m.plexLost(err) },
	}
	// Losing Plex means restarting Plex, not replacing the pod.
	//
	// Plex can die without the supervisor learning of it — it runs under a
	// subreaper that stays alive while any descendant does, so cmd.Wait blocks
	// and OnUnexpectedExit never fires — and the health watch is what notices.
	// Replacing the pod for that cost about forty seconds and re-ran the init
	// script and the election on the way back, for a process that takes
	// seconds to start.
	//
	// The pod is still replaced when Plex cannot stay up, because a pod
	// restarting a process that dies again immediately serves nothing while
	// looking like it is coping.
	var restarts plexRestarts
	var giveUp sync.Once
	m.onPlexLost = func(err error) {
		if restarts.record(time.Now()) {
			giveUp.Do(func() {
				logger.Error("Plex keeps dying; exiting so the pod restarts",
					"restarts", plexRestartLimit, "within", plexRestartWindow, "error", err)
				m.shutdown(context.Background())
				os.Exit(1)
			})
			return
		}
		logger.Error("Plex stopped serving; restarting it", "error", err)
		if err := m.sup.Restart(ctx); err != nil {
			// Nothing else is going to fix this, and a pod that cannot start
			// Plex should not stay up pretending otherwise.
			giveUp.Do(func() {
				logger.Error("cannot restart Plex; exiting so the pod restarts", "error", err)
				m.shutdown(context.Background())
				os.Exit(1)
			})
		}
	}

	go m.serveProbes()
	if err := m.startExecServers(ctx); err != nil {
		logger.Error("start job listeners", "error", err)
		return 1
	}
	m.runPlex(ctx)

	<-ctx.Done()
	logger.Info("shutting down")
	stopCtx, cancel := context.WithTimeout(context.Background(), cfg.DrainTimeout+defaultGrace+5*time.Second)
	defer cancel()
	m.shutdown(stopCtx)
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

// patchPod applies a strategic merge patch to this pod.
func (m *Manager) patchPod(ctx context.Context, payload []byte) (any, error) {
	return m.K8sClient.CoreV1().Pods(m.Config.Namespace).
		Patch(ctx, m.Config.PodName, types.StrategicMergePatchType, payload, metav1.PatchOptions{})
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
