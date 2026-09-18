package remoteexec

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	pb "github.com/mediactl/clusterplex/proto"
)

// Service adapts an execute function to the gRPC ManagerServer interface. The
// same type serves the local unix socket (fed by the shim) and the worker port
// (fed by the leader); only the Execute function differs.
type Service struct {
	pb.UnimplementedManagerServer
	Execute func(ctx context.Context, req *pb.ExecRequest, sink Sink) error
	// OnActive, when set, is called with +1 when a job starts and -1 when it ends.
	OnActive func(delta int)
}

// ExecuteRemote implements pb.ManagerServer.
func (s *Service) ExecuteRemote(req *pb.ExecRequest, stream pb.Manager_ExecuteRemoteServer) error {
	ctx := otel.GetTextMapPropagator().Extract(stream.Context(), propagation.MapCarrier(req.GetTraceHeaders()))
	ctx, span := otel.Tracer("clusterplex").Start(ctx, "ExecuteRemote")
	defer span.End()
	span.SetAttributes(attribute.String("plex.target_binary", req.GetTargetBinary()))

	if s.OnActive != nil {
		s.OnActive(1)
		defer s.OnActive(-1)
	}
	return s.Execute(ctx, req, stream)
}
