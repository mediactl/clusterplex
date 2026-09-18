// The shim stands in for the Plex helper binaries (transcoder, scanner,
// commercial skipper, relay). Plex executes it as if it were the real binary;
// it forwards the invocation to the manager over a unix socket and relays the
// output and exit status back, so the manager can run the job wherever it
// likes.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/mediactl/clusterplex/proto"
)

const defaultSocket = "/var/run/clusterplex.sock"

func main() {
	// Plex stops a transcode by signalling the shim; cancelling the context
	// ends the stream and the manager kills the real process.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	socket := os.Getenv("CLUSTERPLEX_SOCKET")
	if socket == "" {
		socket = defaultSocket
	}
	code := run(ctx, socket, os.Args[0], os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run asks the manager to execute the binary this shim stands in for, copies
// the streamed output to stdout and stderr, and returns the exit code to use.
func run(ctx context.Context, socket, argv0 string, args []string, stdout, stderr io.Writer) int {
	target := filepath.Base(argv0)
	ctx, span := otel.Tracer("plex-shim").Start(ctx, "ShimInterceptor")
	defer span.End()

	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fail(stderr, target, socket, err)
	}
	defer func() { _ = conn.Close() }()

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	cwd, _ := os.Getwd()

	stream, err := pb.NewManagerClient(conn).ExecuteRemote(ctx, &pb.ExecRequest{
		TargetBinary: target,
		Args:         args,
		Env:          envMap(),
		TraceHeaders: carrier,
		Cwd:          cwd,
	})
	if err != nil {
		return fail(stderr, target, socket, err)
	}

	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return fail(stderr, target, socket, errors.New("manager closed the stream without an exit status"))
		}
		if err != nil {
			return fail(stderr, target, socket, err)
		}
		if len(m.StdoutChunk) > 0 {
			_, _ = stdout.Write(m.StdoutChunk)
		}
		if len(m.StderrChunk) > 0 {
			_, _ = stderr.Write(m.StderrChunk)
		}
		if m.IsFinished {
			return int(m.ExitCode)
		}
	}
}

func fail(stderr io.Writer, target, socket string, err error) int {
	_, _ = fmt.Fprintf(stderr, "clusterplex shim: %s: %v (manager socket %s)\n", target, err, socket)
	return 1
}

func envMap() map[string]string {
	env := make(map[string]string)
	for _, e := range os.Environ() {
		if k, v, ok := strings.Cut(e, "="); ok {
			env[k] = v
		}
	}
	return env
}
