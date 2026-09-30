package main

import (
	"context"
	"errors"
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
	shim, worker, err := m.execServices()
	if err != nil {
		return err
	}

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
	pb.RegisterManagerServer(m.shimServer, shim)
	m.workerServer = grpc.NewServer()
	pb.RegisterManagerServer(m.workerServer, worker)

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

// execServices builds what the two job listeners serve: the shim socket's
// dispatcher and the worker port's plain executor. Both run their jobs through
// one local Executor.
func (m *Manager) execServices() (shim, worker *remoteexec.Service, err error) {
	cfg := m.Config
	// Every job starts inside Plex's network namespace, where 127.0.0.1:32400
	// is Plex itself. A helper calls Plex back there -- the scanner allocates
	// a changestamp, the transcoder posts its progress -- and in the pod
	// namespace that address is the proxy, so Plex sees the call arrive from
	// its link subnet (169.254.1.1) instead of from loopback. It still admits
	// the owner's token, but analysis Plex starts itself carries a transient
	// local one, which Plex honours only from loopback: it asks plex.tv about
	// the "unknownToken", treats the caller as a guest and answers 401, and
	// the scanner exits 0 having analysed nothing. There is no fallback to
	// the pod namespace for that reason: a job started there does nothing and
	// says so nowhere but in Plex's own log.
	//
	// That includes jobs the worker port takes from another pod's Plex. Their
	// loopback references were rewritten to that pod's address (RewriteArgs),
	// which Plex's namespace reaches exactly as the pod's does: it resolves
	// through the pod's resolv.conf and leaves through the masquerade, which
	// rewrites everything not bound for the link itself -- the same path Plex
	// takes to PostgreSQL. Anything the rewrite does not cover still means
	// this pod's Plex, which is reached as loopback only from its namespace.
	// A non-leader's plex.tv blocklist applies there too, and a helper has no
	// business with plex.tv. One executor for both listeners keeps it so.
	if m.startInPlexNS == nil {
		return nil, nil, errors.New("no Plex network namespace to start jobs in: a helper started outside it " +
			"reaches the proxy instead of Plex on 127.0.0.1:32400")
	}
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
		Start:  m.startInPlexNS,
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
	return &remoteexec.Service{Execute: dispatcher.Execute, OnActive: active},
		&remoteexec.Service{Execute: local.Run, OnActive: active}, nil
}
