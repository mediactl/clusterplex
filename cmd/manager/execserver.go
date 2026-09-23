package main

import (
	"context"
	"fmt"
	"net"
	"os"

	"google.golang.org/grpc"

	"github.com/mediactl/clusterplex/pkg/remoteexec"
	pb "github.com/mediactl/clusterplex/proto"
)

// startExecServers opens the two job listeners. The unix socket takes jobs
// from the shim that Plex spawns and dispatches them; the TCP port takes jobs
// from the leader and always runs them here. Every pod runs both, so a pod
// can become leader, or be asked to work, without restarting.
func (m *Manager) startExecServers(ctx context.Context) error {
	cfg := m.Config
	// The same environment Plex is started with. A helper reaches the library
	// only if it loads the interposer, and the request cannot be relied on to
	// carry it: the shim removes LD_PRELOAD from Plex's environment once it
	// has loaded, and re-injects it only for a scanner Plex execs itself —
	// never for one that arrives here through our own shim. Without this the
	// scanner opens an empty local SQLite, analyses nothing and exits 0, and
	// playback fails with "video has neither a video stream nor an audio
	// stream" because media_streams was never written.
	//
	// It goes to every job because every job is a Plex binary: Executor
	// resolves the target inside BinDir with a .real suffix and runs nothing
	// else. The interposer being wrong for an ordinary glibc program is why
	// the shim scrubs it broadly; none of those programs runs here.
	local := &remoteexec.Executor{
		BinDir: cfg.BinDir,
		Env:    shimEnv(nil, cfg),
		Logger: m.Logger.With("component", "executor"),
	}
	dispatcher := &remoteexec.Dispatcher{
		Local:   local,
		Workers: &remoteexec.PodWorkerLister{Client: m.K8sClient, Namespace: cfg.Namespace, Self: cfg.PodName, Port: cfg.WorkerPort},
		Dial:    remoteexec.GRPCDialer,
		PMSAddr: cfg.PMSAddr(),
		Logger:  m.Logger.With("component", "dispatcher"),
		OnRoute: func(target, mode string) { m.Metrics.JobsRouted.WithLabelValues(target, mode).Inc() },
	}
	active := func(delta int) { m.Metrics.ActiveJobs.Add(float64(delta)) }

	_ = os.Remove(cfg.Socket)
	unixLis, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return fmt.Errorf("listen on shim socket: %w", err)
	}
	tcpLis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.WorkerPort))
	if err != nil {
		_ = unixLis.Close()
		return fmt.Errorf("listen on worker port: %w", err)
	}

	m.shimServer = grpc.NewServer()
	pb.RegisterManagerServer(m.shimServer, &remoteexec.Service{Execute: dispatcher.Execute, OnActive: active})
	m.workerServer = grpc.NewServer()
	pb.RegisterManagerServer(m.workerServer, &remoteexec.Service{Execute: local.Run, OnActive: active})

	go m.serve("shim socket", m.shimServer, unixLis)
	go m.serve("worker port", m.workerServer, tcpLis)
	context.AfterFunc(ctx, func() {
		m.shimServer.GracefulStop()
		m.workerServer.GracefulStop()
	})
	m.Logger.Info("job listeners started", "socket", cfg.Socket, "worker_port", cfg.WorkerPort)
	return nil
}

func (m *Manager) serve(name string, srv *grpc.Server, lis net.Listener) {
	if err := srv.Serve(lis); err != nil {
		m.Logger.Error("gRPC server stopped", "listener", name, "error", err)
	}
}
