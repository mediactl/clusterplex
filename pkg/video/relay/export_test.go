package relay

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// DialOptionsWith is DialOptions with its keepalive at ka.
func DialOptionsWith(ka keepalive.ClientParameters) []grpc.DialOption { return dialOptions(ka) }
