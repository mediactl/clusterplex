// The proxy is the only thing Plex clients connect to.
//
// It serves direct-play media itself from shared storage, so those bytes never
// pass through Plex and aggregate throughput scales with the number of nodes
// running proxies rather than with one server's network interface. Everything
// else is forwarded to a pod running Plex, chosen by a consistent hash of the
// client so that one client keeps landing on one pod: Plex caches state per
// process and there is no bus to invalidate it.
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
	"strconv"
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
	plexroute "github.com/mediactl/clusterplex/pkg/plex/route"
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
	listen := env("CLUSTERPLEX_PROXY_LISTEN", ":32400")
	plexPort, err := strconv.Atoi(env("CLUSTERPLEX_PMS_PORT", "32400"))
	if err != nil {
		logger.Error("invalid CLUSTERPLEX_PMS_PORT", "error", err)
		return 2
	}
	managerPort, err := strconv.Atoi(env("CLUSTERPLEX_PROBE_PORT", "8080"))
	if err != nil {
		logger.Error("invalid CLUSTERPLEX_PROBE_PORT", "error", err)
		return 2
	}
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

	// Membership comes from the pods themselves rather than from the Lease.
	// The Lease names one holder, which is right for "who talks to plex.tv"
	// and wrong for "who can serve this request": with Plex on several pods
	// there are several answers.
	router := &Router{
		Tracker: &plexroute.PodTracker{
			Client:      client,
			Namespace:   namespace,
			Port:        plexPort,
			ManagerPort: managerPort,
		},
		Logger: logger.With("component", "router"),
	}
	go router.Run(ctx)

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
	await := func(ctx context.Context, sessionKey string) (plexroute.Target, error) {
		if target, ok := router.Locate(sessionKey); ok {
			return target, nil
		}
		waits.Inc()
		return router.Wait(ctx, sessionKey, waitFor)
	}

	handler := &mediaproxy.Handler{
		Upstream: func(ctx context.Context, sessionKey string) (string, error) {
			target, err := await(ctx, sessionKey)
			if err != nil {
				return "", err
			}
			return "http://" + target.Address, nil
		},
		Resolver: &mediaproxy.HTTPResolver{
			// Media is authorized and resolved by the manager beside the Plex
			// that owns this client, so a resolve and the request it is for
			// always reach the same pod.
			Endpoint: func(ctx context.Context) (string, error) {
				target, err := await(ctx, "")
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

	go serveProbes(ctx, logger, probeAddr, router)

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

	logger.Info("proxy listening", "addr", listen, "tls", certFile != "", "namespace", namespace)
	if err := serve(lis, srv, certFile, keyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("proxy stopped", "error", err)
		return 1
	}
	return 0
}

// serveProbes reports readiness. A proxy that cannot see a Plex is taken out
// of the load balancer rather than accepting connections it could only stall.
func serveProbes(ctx context.Context, logger *slog.Logger, addr string, router *Router) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		target, ok := router.Locate("")
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
