package remoteexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/mediactl/clusterplex/proto"
)

// Transcoder is the only helper worth moving off the leader: it is CPU-bound
// and self-contained. The scanner, relay and commercial skipper need the
// server's own filesystem and sockets.
const Transcoder = "Plex Transcoder"

// Worker is a pod able to run jobs.
type Worker struct {
	Name string
	Addr string // host:port of its manager's gRPC listener
}

// WorkerLister finds workers that are ready to take a job.
type WorkerLister interface {
	ListReady(ctx context.Context) ([]Worker, error)
}

// Dialer opens a client to a worker. The returned Closer releases the connection.
type Dialer func(ctx context.Context, addr string) (pb.ManagerClient, io.Closer, error)

// GRPCDialer dials a worker's manager over plaintext gRPC inside the cluster.
func GRPCDialer(_ context.Context, addr string) (pb.ManagerClient, io.Closer, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return pb.NewManagerClient(conn), conn, nil
}

// Dispatcher decides where a shimmed job runs. Eligible jobs go to a ready
// worker in round-robin order; everything else, and anything a worker fails
// to accept, runs on the local Executor.
type Dispatcher struct {
	Local   *Executor
	Workers WorkerLister // nil means never dispatch remotely
	Dial    Dialer
	// PMSAddr is how workers reach this node's Plex Media Server (host:port).
	// Loopback references in the job's arguments are rewritten to it.
	PMSAddr string
	Logger  *slog.Logger
	// OnRoute, when set, is told the target and "local" or "remote" per job.
	OnRoute func(target, mode string)

	next atomic.Uint64
}

// RemoteEligible reports whether a request may run on another node. Transcodes
// that use the EasyAudioEncoder helper depend on a process the server starts
// beside itself, so they stay local until workers learn to run one.
func RemoteEligible(req *pb.ExecRequest) bool {
	return req.GetTargetBinary() == Transcoder && req.GetEnv()["EAE_ROOT"] == ""
}

// Execute runs the job remotely when it can, locally otherwise.
func (d *Dispatcher) Execute(ctx context.Context, req *pb.ExecRequest, sink Sink) error {
	if RemoteEligible(req) {
		if done, err := d.tryRemote(ctx, req, sink); done {
			return err
		}
	}
	d.route(req.GetTargetBinary(), "local")
	return d.Local.Run(ctx, req, sink)
}

// tryRemote reports done=false when no worker took the job and nothing was
// streamed yet, so the caller may still run it locally.
func (d *Dispatcher) tryRemote(ctx context.Context, req *pb.ExecRequest, sink Sink) (bool, error) {
	if d.Workers == nil || d.Dial == nil {
		return false, nil
	}
	if d.PMSAddr == "" {
		d.log().Warn("remote execution disabled: PMSAddr is empty")
		return false, nil
	}
	workers, err := d.Workers.ListReady(ctx)
	if err != nil {
		d.log().Warn("list workers failed, running locally", "error", err)
		return false, nil
	}
	if len(workers) == 0 {
		return false, nil
	}
	w := d.pick(workers)
	log := d.log().With("worker", w.Name, "target", req.GetTargetBinary())

	client, closer, err := d.Dial(ctx, w.Addr)
	if err != nil {
		log.Warn("dial worker failed, running locally", "error", err)
		return false, nil
	}
	defer func() { _ = closer.Close() }()

	stream, err := client.ExecuteRemote(ctx, &pb.ExecRequest{
		TargetBinary: req.GetTargetBinary(),
		Args:         RewriteArgs(req.GetArgs(), d.PMSAddr),
		Env:          req.GetEnv(),
		TraceHeaders: req.GetTraceHeaders(),
		Cwd:          req.GetCwd(),
	})
	if err != nil {
		log.Warn("worker refused job, running locally", "error", err)
		return false, nil
	}

	first := true
	for {
		m, err := stream.Recv()
		if err != nil {
			if first {
				log.Warn("worker rejected job before output, running locally", "error", err)
				return false, nil
			}
			if errors.Is(err, io.EOF) {
				return true, nil
			}
			return true, fmt.Errorf("worker %s: %w", w.Name, err)
		}
		if first {
			d.route(req.GetTargetBinary(), "remote")
			first = false
		}
		if err := sink.Send(m); err != nil {
			return true, err
		}
		if m.GetIsFinished() {
			return true, nil
		}
	}
}

func (d *Dispatcher) pick(workers []Worker) Worker {
	i := d.next.Add(1) - 1
	return workers[int(i%uint64(len(workers)))]
}

func (d *Dispatcher) route(target, mode string) {
	if d.OnRoute != nil {
		d.OnRoute(target, mode)
	}
}

func (d *Dispatcher) log() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}
