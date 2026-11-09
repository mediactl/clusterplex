package net

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBlocklist records what the guard asked the firewall to block.
type fakeBlocklist struct {
	mu      sync.Mutex
	current []netip.Addr
	calls   int
	err     error
}

func (f *fakeBlocklist) Set(_ context.Context, addrs []netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.current = append([]netip.Addr(nil), addrs...)
	return nil
}

func (f *fakeBlocklist) blocked() []netip.Addr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]netip.Addr(nil), f.current...)
}

func resolver(byHost map[string][]string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		addrs, ok := byHost[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		out := make([]netip.Addr, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
}

func guard(list *fakeBlocklist, hosts ...string) *EgressGuard {
	return &EgressGuard{
		Hosts:     hosts,
		Blocklist: list,
		Resolve: resolver(map[string][]string{
			"plex.tv":        {"1.2.3.4", "1.2.3.5"},
			"pubsub.plex.tv": {"5.6.7.8"},
		}),
	}
}

func TestAFollowerBlocksEveryResolvedAddress(t *testing.T) {
	// Several pods holding the plex.tv connection under one server identity
	// makes that identity appear to move between addresses, which breaks
	// remote access. Only the lease holder may talk to it.
	list := &fakeBlocklist{}
	g := guard(list, "plex.tv", "pubsub.plex.tv")

	require.NoError(t, g.Apply(t.Context(), false))

	assert.ElementsMatch(t, []netip.Addr{
		netip.MustParseAddr("1.2.3.4"),
		netip.MustParseAddr("1.2.3.5"),
		netip.MustParseAddr("5.6.7.8"),
	}, list.blocked())
}

func TestTheHolderBlocksNothing(t *testing.T) {
	list := &fakeBlocklist{}
	g := guard(list, "plex.tv")

	require.NoError(t, g.Apply(t.Context(), true))

	assert.Empty(t, list.blocked())
}

func TestApplySurvivesAHostThatWillNotResolve(t *testing.T) {
	// Blocking what we could resolve is better than blocking nothing, and a
	// DNS hiccup must not leave the pod unable to decide.
	list := &fakeBlocklist{}
	g := guard(list, "plex.tv", "does-not-exist.invalid")

	require.NoError(t, g.Apply(t.Context(), false))

	assert.Contains(t, list.blocked(), netip.MustParseAddr("1.2.3.4"))
}

func TestApplyFailsWhenNothingResolves(t *testing.T) {
	// Silently blocking nothing would look like success while every pod still
	// reaches plex.tv.
	list := &fakeBlocklist{}
	g := guard(list, "does-not-exist.invalid")

	require.Error(t, g.Apply(t.Context(), false))
}

func TestApplyReportsAFirewallFailure(t *testing.T) {
	list := &fakeBlocklist{err: errors.New("permission denied")}
	g := guard(list, "plex.tv")

	err := g.Apply(t.Context(), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
}

func TestApplyIsIdempotentForTheSameAddresses(t *testing.T) {
	// The guard re-runs on a timer, and rewriting an unchanged set every few
	// seconds is churn against the kernel for nothing.
	list := &fakeBlocklist{}
	g := guard(list, "plex.tv")

	require.NoError(t, g.Apply(t.Context(), false))
	require.NoError(t, g.Apply(t.Context(), false))

	assert.Equal(t, 1, list.calls)
}

func TestChangingRoleRewritesTheBlocklist(t *testing.T) {
	list := &fakeBlocklist{}
	g := guard(list, "plex.tv")

	require.NoError(t, g.Apply(t.Context(), false))
	require.NoError(t, g.Apply(t.Context(), true))

	assert.Equal(t, 2, list.calls)
	assert.Empty(t, list.blocked())
}

func TestDefaultHostsCoverPlexsOwnServices(t *testing.T) {
	assert.Contains(t, DefaultBlockedHosts, "plex.tv")
	assert.Contains(t, DefaultBlockedHosts, "pubsub.plex.tv")
}

// TestAPodWithoutTheLeaseStillSignsClientsIn: PMS checks a token it has not
// seen at servers.plex.tv, so blocking it would refuse every new client's
// sign-in on the pods without the lease (the kind rig, 2026-11-09).
func TestAPodWithoutTheLeaseStillSignsClientsIn(t *testing.T) {
	for _, h := range []string{"servers.plex.tv", "epg.provider.plex.tv"} {
		assert.NotContains(t, DefaultBlockedHosts, h)
	}
}

func TestEgressGuardBlocksTheLeaderTooWhenNobodyMayReachPlexTV(t *testing.T) {
	// An operator needs to be able to keep every pod away from plex.tv, lease
	// holder included: a cluster that is not meant to be published, or one
	// being kept off it while something is investigated.
	//
	// Packets are dropped, which is the case Plex is built for -- a server
	// that cannot reach plex.tv still serves. Pointing the names elsewhere
	// instead does not work: at loopback Plex reaches its own HTTP server and
	// dies on the 400 it gets back, and at an unroutable address it dies
	// parsing what comes of the timeout.
	//
	// This is a deliberate degradation -- no claiming, no remote access -- so
	// it is off unless asked for.
	spy := &fakeBlocklist{}
	g := &EgressGuard{
		Blocklist: spy,
		BlockAll:  true,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("198.51.100.7")}, nil
		},
	}

	require.NoError(t, g.Apply(context.Background(), true))

	assert.NotEmpty(t, spy.current, "the lease holder must be blocked as well")
}

// A name DNS knows no address for (metrics.plex.tv has no IPv4 record) is
// nothing to block: it is no failure and warns nothing, while a name that
// fails to resolve still warns.
func TestAHostWithNoAddressIsNoFailure(t *testing.T) {
	var logs bytes.Buffer
	list := &fakeBlocklist{}
	g := guard(list, "plex.tv", "metrics.plex.tv", "does-not-exist.invalid")
	known := g.Resolve
	g.Resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host == "metrics.plex.tv" {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return known(ctx, host)
	}
	g.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	require.NoError(t, g.Apply(t.Context(), false))

	assert.Contains(t, list.blocked(), netip.MustParseAddr("1.2.3.4"))
	assert.NotContains(t, logs.String(), "metrics.plex.tv")
	assert.Contains(t, logs.String(), "does-not-exist.invalid", "a lookup that fails still warns")

	only := guard(&fakeBlocklist{}, "metrics.plex.tv")
	only.Resolve = g.Resolve
	require.Error(t, only.Apply(t.Context(), false), "nothing resolved is still nothing filtered")
}
