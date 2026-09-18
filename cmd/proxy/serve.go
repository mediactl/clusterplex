package main

import (
	"errors"
	"net"
	"net/http"
)

// serve runs srv on lis, with TLS when a certificate is configured.
//
// Plex clients prefer a secure connection and will decline a server that only
// offers plain HTTP, so a proxy meant to replace the server's own endpoint has
// to be able to present a certificate. Terminating here, rather than reusing
// Plex's own plex.direct key material, is what lets the deployment advertise
// its own hostname.
func serve(lis net.Listener, srv *http.Server, certFile, keyFile string) error {
	switch {
	case certFile == "" && keyFile == "":
		return srv.Serve(lis)
	case certFile == "" || keyFile == "":
		return errors.New("a TLS certificate needs both a cert and a key")
	default:
		return srv.ServeTLS(lis, certFile, keyFile)
	}
}
