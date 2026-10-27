package worker

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// ServerOptionsWith is ServerOptions with its keepalive at ka.
func ServerOptionsWith(ka keepalive.ServerParameters) []grpc.ServerOption { return serverOptions(ka) }
