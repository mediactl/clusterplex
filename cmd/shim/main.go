package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "plex-supervisor/proto"
)

func main() {
	ctx := context.Background()
	tracer := otel.Tracer("plex-shim")
	ctx, span := tracer.Start(ctx, "ShimInterceptor")
	defer span.End()

	// Determine WHICH binary Plex is attempting to execute
	targetBinary := filepath.Base(os.Args[0])
	args := os.Args[1:]

	conn, err := grpc.DialContext(ctx, "unix:///var/run/plex-supervisor.sock",
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		os.Exit(1)
	}
	defer conn.Close()

	client := pb.NewSupervisorClient(conn)

	// Propagate OTel context into gRPC headers map
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	req := &pb.ExecRequest{
		TargetBinary: targetBinary,
		Args:         args,
		Env:          getEnvMap(),
		TraceHeaders: carrier,
	}

	stream, err := client.ExecuteRemote(ctx, req)
	if err != nil {
		os.Exit(1)
	}

	var exitCode int32 = 1
	for {
		logData, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if len(logData.StdoutChunk) > 0 {
			os.Stdout.Write(logData.StdoutChunk)
		}
		if len(logData.StderrChunk) > 0 {
			os.Stderr.Write(logData.StderrChunk)
		}
		if logData.IsFinished {
			exitCode = logData.ExitCode
			break
		}
	}
	os.Exit(int(exitCode))
}

func getEnvMap() map[string]string {
	envMap := make(map[string]string)
	for _, e := range os.Environ() {
		pair := strings.SplitN(e, "=", 2)
		if len(pair) == 2 {
			envMap[pair[0]] = pair[1]
		}
	}
	return envMap
}
