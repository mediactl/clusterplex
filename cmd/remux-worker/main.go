// Command remux-worker serves cluster-plex's Remux gRPC service on the
// remux pool: Plex Web's DASH streams, remuxed in process with ffgo
// (docs/superpowers/specs/2026-10-01-browser-remux-transcoder-design.md).
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
	"syscall"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"

	"github.com/mediactl/clusterplex/pkg/remux/cache"
	"github.com/mediactl/clusterplex/pkg/remux/pipeline"
	"github.com/mediactl/clusterplex/pkg/remux/worker"
	"github.com/mediactl/clusterplex/proto/remuxpb"
)

func main() {
	if err := run(); err != nil {
		slog.Error("remux-worker", "error", err)
		os.Exit(1)
	}
}

func run() error {
	listen := pflag.String("listen", ":50052", "address serving the Remux gRPC service")
	probe := pflag.String("probe", ":8080", "address serving /healthz and /readyz")
	dir := pflag.String("cache-dir", "/cache", "worker-local directory holding the remux cache")
	maxBytes := pflag.Int64("cache-max-bytes", 50<<30, "size the cache is evicted down to; 0 keeps everything")
	pflag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if err := ffgo.Init(); err != nil {
		return fmt.Errorf("load FFmpeg: %w", err)
	}
	if _, avc, _ := ffgo.Version(); avc>>16 != 63 {
		return fmt.Errorf("libavcodec %d is not FFmpeg 9's (63)", avc>>16)
	}
	if d := ffgo.Diagnose(); !d.ShimLoaded {
		return fmt.Errorf("no ffgo shim for FFmpeg 9 loaded: %s", d.ShimError)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	srv := grpc.NewServer()
	remuxpb.RegisterRemuxServer(srv, &worker.Server{
		Cache:  &cache.Cache{Dir: *dir, MaxBytes: *maxBytes},
		Run:    pipeline.Run,
		Logger: log,
	})
	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/readyz", ok)
	hs := &http.Server{Addr: *probe, Handler: mux}
	go func() { _ = hs.ListenAndServe() }()
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
		_ = hs.Close()
	}()
	log.Info("serving", "listen", *listen, "cache", *dir)
	if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}
