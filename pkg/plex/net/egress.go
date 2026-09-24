package plexnet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
)

// DefaultBlockedHosts are the Plex services only one pod may talk to.
//
// Every pod shares one server identity. If several hold the connection to
// plex.tv at once, that identity appears to move between addresses and remote
// access breaks. The pod holding the lease keeps the connection; the rest are
// kept away from these names.
//
// Blocking by resolved address rather than by name because the filter is a
// packet filter: the names are resolved periodically and the set updated.
var DefaultBlockedHosts = []string{
	"plex.tv",
	"pubsub.plex.tv",
	"plex.direct",
	"metrics.plex.tv",
}

// Blocklist is the firewall set of addresses Plex may not reach.
type Blocklist interface {
	Set(ctx context.Context, addrs []netip.Addr) error
}

// EgressGuard keeps that set in step with who holds the lease.
//
// It filters only these names. Plex still reaches cluster DNS, the media
// itself and metadata providers such as TMDB, which is what lets a pod that
// cannot talk to plex.tv still serve.
type EgressGuard struct {
	// Hosts to block; DefaultBlockedHosts when empty.
	Hosts []string
	// BlockAll keeps every pod away from these names, the lease holder
	// included, rather than only the pods that do not hold it.
	//
	// For when nothing should reach plex.tv at all: a cluster that is not
	// meant to be published, or one being kept off it while something is
	// investigated. Plex is built to run without it -- the packets are
	// dropped, which reads to Plex as an ordinary outage -- but nothing can
	// claim the server or serve remotely while it is set, so it is not the
	// default.
	BlockAll  bool
	Blocklist Blocklist
	// Resolve looks a hostname up; net.DefaultResolver when nil.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	Logger  *slog.Logger

	mu      sync.Mutex
	applied []netip.Addr
	valid   bool
}

// Apply blocks or unblocks the hosts according to whether this pod holds the
// lease. It is safe to call repeatedly: an unchanged set is not rewritten.
func (g *EgressGuard) Apply(ctx context.Context, holdsLease bool) error {
	want, err := g.wanted(ctx, holdsLease)
	if err != nil {
		return err
	}

	g.mu.Lock()
	unchanged := g.valid && slices.Equal(g.applied, want)
	g.mu.Unlock()
	if unchanged {
		return nil
	}

	if err := g.Blocklist.Set(ctx, want); err != nil {
		return fmt.Errorf("update egress blocklist: %w", err)
	}

	g.mu.Lock()
	g.applied, g.valid = want, true
	g.mu.Unlock()

	switch {
	case len(want) == 0:
		g.log().Info("this pod holds the plex.tv lease; egress is open")
	case g.BlockAll:
		g.log().Info("no pod may reach Plex's own services; blocking them here too",
			"addresses", len(want))
	default:
		g.log().Info("this pod does not hold the plex.tv lease; blocking Plex's own services",
			"addresses", len(want))
	}
	return nil
}

// wanted is the set of addresses to block, sorted so that an unchanged set
// compares equal.
func (g *EgressGuard) wanted(ctx context.Context, holdsLease bool) ([]netip.Addr, error) {
	if holdsLease && !g.BlockAll {
		return nil, nil
	}

	hosts := g.Hosts
	if len(hosts) == 0 {
		hosts = DefaultBlockedHosts
	}
	resolve := g.Resolve
	if resolve == nil {
		resolve = lookup
	}

	var addrs []netip.Addr
	var failures []string
	for _, host := range hosts {
		found, err := resolve(ctx, host)
		if err != nil {
			// Blocking what did resolve beats blocking nothing, so one name
			// failing is not fatal on its own.
			failures = append(failures, host)
			g.log().Warn("cannot resolve a host to block", "host", host, "error", err)
			continue
		}
		addrs = append(addrs, found...)
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolved none of %s, refusing to report egress as filtered",
			strings.Join(failures, ", "))
	}

	slices.SortFunc(addrs, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(addrs), nil
}

func lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, errors.New("no addresses")
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.Unmap())
	}
	return out, nil
}

func (g *EgressGuard) log() *slog.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return slog.Default()
}
