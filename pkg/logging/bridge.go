package logging

import (
	"log/slog"

	"github.com/go-logr/logr"
)

// LogrBridge adapts l to a logr.Logger via logr.FromSlogHandler.
//
// controller-runtime speaks logr, not slog. A service passes the result of
// this call to ctrl.SetLogger once at startup so controller-runtime's own
// log lines join the same slog stream, in the same format, as the rest of
// the service, instead of going to a separately configured logr backend.
func LogrBridge(l *slog.Logger) logr.Logger {
	return logr.FromSlogHandler(l.Handler())
}
