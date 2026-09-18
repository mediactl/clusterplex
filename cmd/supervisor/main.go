package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/trace"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/mediactl/clusterplex/pkg/telemetry"
)

// Supervisor holds injected dependencies
type Supervisor struct {
	Logger    *slog.Logger
	Tracer    trace.Tracer
	Metrics   *telemetry.Metrics
	K8sClient *kubernetes.Clientset

	Namespace string
	PodName   string

	mu         sync.RWMutex
	isLeader   bool
	isReady    bool
	isStarting bool
	pmsCmd     *exec.Cmd
}

func main() {
	// 1. Dependency Setup
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	tracer, metrics := telemetry.InitTelemetry()

	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("Failed to load K8s config", slog.Any("error", err))
		os.Exit(1)
	}

	sup := &Supervisor{
		Logger:     logger,
		Tracer:     tracer,
		Metrics:    metrics,
		K8sClient:  kubernetes.NewForConfigOrDie(k8sConfig),
		Namespace:  os.Getenv("POD_NAMESPACE"),
		PodName:    os.Getenv("POD_NAME"),
		isStarting: true,
	}

	// 2. Start Probes & Metrics Server
	go sup.startHTTPServer()

	// 3. Start Leader Election
	sup.runLeaderElection(context.Background())
}

// startHTTPServer sets up the standard K8s probes and Prometheus endpoints
func (s *Supervisor) startHTTPServer() {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	// Liveness: Is the supervisor daemon deadlocked?
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Readiness: Can this node accept traffic (UI or Worker tasks)?
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		ready := s.isReady
		s.mu.RUnlock()
		if ready {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ready"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})

	// Startup: Has initial initialization finished?
	mux.HandleFunc("/startupz", func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		starting := s.isStarting
		s.mu.RUnlock()
		if !starting {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})

	s.Logger.Info("Starting Probe & Metrics server on :8080")
	http.ListenAndServe(":8080", mux)
}

func (s *Supervisor) runLeaderElection(ctx context.Context) {
	s.Logger.InfoContext(ctx, "Starting Kubernetes Lease Leader Election")

	// The Lease API (coordination.k8s.io) is the modern standard for K8s leader election
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      "plex-supervisor-lock",
			Namespace: s.Namespace,
		},
		Client: s.K8sClient.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: s.PodName,
		},
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				s.Logger.InfoContext(ctx, "Acquired leadership. Starting Plex Media Server.")
				s.Metrics.LeaderStatus.Set(1)

				s.mu.Lock()
				s.isLeader = true
				s.isStarting = false
				s.isReady = true
				s.mu.Unlock()

				s.pmsCmd = exec.CommandContext(ctx, "/usr/lib/plexmediaserver/Plex Media Server")
				s.pmsCmd.Stdout = os.Stdout
				s.pmsCmd.Stderr = os.Stderr
				s.pmsCmd.Start()
			},
			OnStoppedLeading: func() {
				s.Logger.WarnContext(ctx, "Lost leadership. Shutting down to protect database.")
				s.Metrics.LeaderStatus.Set(0)
				if s.pmsCmd != nil && s.pmsCmd.Process != nil {
					s.pmsCmd.Process.Kill()
				}
				os.Exit(0)
			},
			OnNewLeader: func(identity string) {
				if identity != s.PodName {
					s.Logger.InfoContext(ctx, "Node elected as worker", slog.String("leader", identity))
					s.mu.Lock()
					s.isLeader = false
					s.isStarting = false
					s.isReady = true // Worker is ready to receive tasks
					s.mu.Unlock()
				}
			},
		},
	})
}
