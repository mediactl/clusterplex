// Command video-worker serves cluster-plex's Video gRPC service on the GPU
// pool: Plex's video conversions, decoded, scaled and encoded in process
// with ffgo on the pod's Intel device, and streamed back to the relay
// (docs/superpowers/specs/2026-10-26-gpu-video-transcode-pool-design.md).
// It writes nothing to disk but a streamed subtitle, in --inputs-dir.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/obinnaokechukwu/ffgo/avcodec"
	"github.com/obinnaokechukwu/ffgo/avfilter"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"

	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/gpu"
	"github.com/mediactl/clusterplex/pkg/video/pipeline"
	"github.com/mediactl/clusterplex/pkg/video/worker"
	"github.com/mediactl/clusterplex/proto/videopb"
)

// minShimAPI is the ffgo shim API the pinned ffgo forces keyframes through
// (2 = v0.0.0-clustarr.14).
const minShimAPI = 2

// checkRuntime refuses an FFmpeg other than 9 (libavcodec 63) or a shim
// older than minShim: ffgo reports a shim that predates its API as 0.
func checkRuntime(version func() (uint32, uint32, uint32), shimAPI func() int, minShim int) error {
	if _, avc, _ := version(); avc>>16 != 63 {
		return fmt.Errorf("libavcodec %d is not FFmpeg 9's (63)", avc>>16)
	}
	if n := shimAPI(); n < minShim {
		return fmt.Errorf("ffgo shim API %d is older than %d", n, minShim)
	}
	return nil
}

// probes serves /healthz, always, and /readyz while ready: the device
// proved itself.
func probes(ready func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/metrics", promhttp.Handler())
	return mux
}

// selfCheckEncoders and selfCheckFilters are what the pool's image must
// carry; selfCheckDriver is Intel's media driver.
var (
	selfCheckEncoders = []string{"h264_qsv", "libx264", "aac"}
	selfCheckFilters  = []string{"vpp_qsv", "hwmap", "hwupload", "subtitles", "bwdif", "zscale", "tonemap", "scale"}
	selfCheckDriver   = "/usr/lib/x86_64-linux-gnu/dri/iHD_drv_video.so"
	selfCheckFiles    = []string{selfCheckDriver, "/usr/share/fonts/noto/NotoSans-Medium.ttf", "/etc/fonts/fonts.conf"}
)

// selfCheck prints the runtime and what the image carries, and fails on
// anything missing. It needs no GPU.
func selfCheck(w io.Writer) error {
	_, avc, _ := ffgo.Version()
	_, _ = fmt.Fprintf(w, "libavcodec %d\nshim api %d\n", avc>>16, ffgo.ShimAPIVersion())
	var missing []string
	for _, e := range selfCheckEncoders {
		found := avcodec.FindEncoderByName(e) != nil
		_, _ = fmt.Fprintf(w, "encoder %s: %s\n", e, map[bool]string{true: "found", false: "missing"}[found])
		if !found {
			missing = append(missing, e)
		}
	}
	for _, f := range selfCheckFilters {
		found := avfilter.GetByName(f) != nil
		_, _ = fmt.Fprintf(w, "filter %s: %s\n", f, map[bool]string{true: "found", false: "missing"}[found])
		if !found {
			missing = append(missing, f)
		}
	}
	for _, f := range selfCheckFiles {
		_, err := os.Stat(f)
		_, _ = fmt.Fprintf(w, "file %s: %s\n", f, map[bool]string{true: "present", false: "missing"}[err == nil])
		if err != nil {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing %v", missing)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("video-worker", "error", err)
		os.Exit(1)
	}
}

func run() error {
	listen := pflag.String("listen", ":50053", "address serving the Video gRPC service")
	probe := pflag.String("probe", ":8080", "address serving /healthz, /readyz and /metrics")
	maxJobs := pflag.Int("max-jobs", 3, "conversions at once; a whole-file job never takes the last free slot")
	node := pflag.String("render-node", "/dev/dri/renderD128", "the Intel GPU's render node")
	encoder := pflag.String("encoder", "h264_qsv", "h264_qsv on the GPU; libx264 encodes and scales on the CPU")
	inputsDir := pflag.String("inputs-dir", "/run/video-inputs", "memory-backed directory a streamed subtitle is held in for burn-in")
	fontsDir := pflag.String("fonts-dir", "/usr/share/fonts/noto", "the fonts burned-in text is drawn in (Noto Sans)")
	check := pflag.Bool("self-check", false, "print the runtime and what the image carries, then exit")
	pflag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if err := ffgo.Init(); err != nil {
		return fmt.Errorf("load FFmpeg: %w", err)
	}
	if d := ffgo.Diagnose(); !d.ShimLoaded {
		return fmt.Errorf("no ffgo shim for FFmpeg 9 loaded: %s", d.ShimError)
	}
	if err := checkRuntime(ffgo.Version, ffgo.ShimAPIVersion, minShimAPI); err != nil {
		return err
	}
	if *check {
		return selfCheck(os.Stdout)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	active := promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "clusterplex_video_worker_active_jobs", Help: "Conversions this pod runs, by output (hls, dash, file)",
	}, []string{"kind"})
	speed := promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "clusterplex_video_worker_encode_speed", Help: "A finished conversion's media seconds per second it ran",
		Buckets: []float64{0.5, 1, 1.5, 2, 3, 5, 8, 12, 20, 40},
	})
	var measured atomic.Bool
	srv := &worker.Server{
		Slots: worker.NewSlots(*maxJobs), Run: pipeline.Run, Logger: log, Measured: measured.Load,
		Options: pipeline.Options{Encoder: *encoder, RenderNode: *node, InputsDir: *inputsDir, FontsDir: *fontsDir},
		Active:  func(k video.OutputKind, d int) { active.WithLabelValues(string(k)).Add(float64(d)) },
		Speed:   speed.Observe,
	}
	hs := &http.Server{Addr: *probe, Handler: probes(srv.Ready)}
	go func() { _ = hs.ListenAndServe() }()
	if *encoder == "libx264" {
		measured.Store(true)
	} else {
		caps, err := gpu.Measure(ctx, *node)
		if err != nil {
			// The pod stays unready and takes no work (spec §3).
			log.Error("the device failed its measure; the pod takes no work", "node", *node, "error", err)
		} else {
			srv.Options.Caps = &caps
			measured.Store(true)
			log.Info("device measured", "node", *node, "decode", caps.Decode, "deinterlace", caps.Deinterlace,
				"tonemap", caps.ToneMap, "took", caps.Took)
		}
	}

	gs := grpc.NewServer(worker.ServerOptions()...)
	videopb.RegisterVideoServer(gs, srv)
	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
		_ = hs.Close()
	}()
	log.Info("serving", "listen", *listen, "encoder", *encoder, "max_jobs", *maxJobs)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}
