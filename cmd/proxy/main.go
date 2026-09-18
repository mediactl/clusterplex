// The proxy is the only thing Plex clients connect to.
//
// It serves direct-play media itself from shared storage, so those bytes never
// pass through Plex and aggregate throughput scales with the number of nodes
// running proxies rather than with one server's network interface. Everything
// else is forwarded to whichever pod currently runs Plex, which it learns from
// the Kubernetes Lease.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/mediactl/clusterplex/pkg/mediaproxy"
	"github.com/mediactl/clusterplex/pkg/plexroute"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	namespace := env("POD_NAMESPACE", "")
	if namespace == "" {
		logger.Error("POD_NAMESPACE must be set (use the downward API)")
		return 2
	}
	leaseName := env("CLUSTERPLEX_LEASE_NAME", "cluster-plex-litefs")
	listen := env("CLUSTERPLEX_PROXY_LISTEN", ":32400")
	probeAddr := env("CLUSTERPLEX_PROBE_LISTEN", ":8080")
	certFile := env("CLUSTERPLEX_TLS_CERT", "")
	keyFile := env("CLUSTERPLEX_TLS_KEY", "")
	waitFor, err := time.ParseDuration(env("CLUSTERPLEX_FAILOVER_GRACE", "30s"))
	if err != nil {
		logger.Error("invalid CLUSTERPLEX_FAILOVER_GRACE", "error", err)
		return 2
	}

	client, err := newClient()
	if err != nil {
		logger.Error("load kubernetes config", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	tracker := &plexroute.Tracker{
		Client:    client,
		Namespace: namespace,
		LeaseName: leaseName,
		Logger:    logger.With("component", "tracker"),
	}
	go tracker.Run(ctx)

	bytesServed := promauto.NewCounter(prometheus.CounterOpts{
		Name: "clusterplex_proxy_media_bytes_total",
		Help: "Media bytes served directly by the proxy. Plex cannot observe these and has no API to be told of them.",
	})
	waits := promauto.NewCounter(prometheus.CounterOpts{
		Name: "clusterplex_proxy_upstream_waits_total",
		Help: "Requests that had to wait for a Plex Media Server to become available.",
	})

	// Waiting rather than failing is what turns a failover into a pause for
	// the client instead of an error.
	await := func(ctx context.Context) (plexroute.Target, error) {
		if target, ok := tracker.Current(); ok {
			return target, nil
		}
		waits.Inc()
		return tracker.Wait(ctx, waitFor)
	}

	handler := &mediaproxy.Handler{
		Upstream: func(ctx context.Context) (string, error) {
			target, err := await(ctx)
			if err != nil {
				return "", err
			}
			return "http://" + target.Address, nil
		},
		Resolver: &mediaproxy.HTTPResolver{
			Endpoint: func(ctx context.Context) (string, error) {
				target, err := await(ctx)
				if err != nil {
					return "", err
				}
				return target.Manager, nil
			},
		},
		Timeout:       waitFor + time.Minute,
		Logger:        logger.With("component", "proxy"),
		OnBytesServed: func(n int64) { bytesServed.Add(float64(n)) },
	}

	go serveProbes(ctx, logger, probeAddr, tracker)

	srv := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		logger.Error("listen", "addr", listen, "error", err)
		return 1
	}

	logger.Info("proxy listening", "addr", listen, "tls", certFile != "", "lease", leaseName, "namespace", namespace)
	if err := serve(lis, srv, certFile, keyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("proxy stopped", "error", err)
		return 1
	}
	return 0
}

// serveProbes reports readiness. A proxy that cannot see a Plex is taken out
// of the load balancer rather than accepting connections it could only stall.
func serveProbes(ctx context.Context, logger *slog.Logger, addr string, tracker *plexroute.Tracker) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		target, ok := tracker.Current()
		if !ok {
			http.Error(w, "no Plex Media Server is available", http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintf(w, "routing to %s", target.Pod)
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("probe server stopped", "error", err)
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func newClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = os.ExpandEnv("$HOME/.kube/config")
		}
		if cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig); err != nil {
			return nil, err
		}
	}
	return kubernetes.NewForConfig(cfg)
}
