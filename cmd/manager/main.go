package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/trace"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/mediactl/clusterplex/pkg/telemetry"
)

// Manager holds injected dependencies
type Manager struct {
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
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = os.ExpandEnv("$HOME/.kube/config")
		}
		var buildErr error
		k8sConfig, buildErr = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if buildErr != nil {
			logger.Error("Failed to load K8s config", slog.Any("error", buildErr))
			os.Exit(1)
		}
	}

	sup := &Manager{
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

	// 3. Start LiteFS
	if err := sup.startLiteFS(context.Background()); err != nil {
		logger.Error("Failed to start LiteFS", slog.Any("error", err))
		os.Exit(1)
	}

	// Block forever
	select {}
}

// startHTTPServer sets up the standard K8s probes and Prometheus endpoints
func (s *Manager) startHTTPServer() {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	// Liveness: Is the manager daemon deadlocked?
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
