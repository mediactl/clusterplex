//go:build netns

// See network_netns_test.go: these provision real network namespaces and are
// run by `make test-netns`.
package plexnet

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

func TestProvisioningSucceedsWhenAPreviousContainerLeftItsLinkBehind(t *testing.T) {
	// The veth lives in the pod's network namespace, and that namespace
	// outlives the container inside it. A container that restarts therefore
	// finds its predecessor's link still there, and LinkAdd refuses:
	//
	//	provision Plex network namespace:
	//	  create veth pair plex0/plex1: file exists
	//
	// Provisioning is fatal, so the pod then crash-loops for ever — one
	// restart for any reason and it never comes back. plex-0 did exactly
	// that, five times over, while plex-1 and plex-2 stayed up beside it.
	//
	// Anything already holding our name is ours and stale by definition: the
	// name is fixed, the namespace belongs to this pod alone, and nothing
	// else creates it.
	cfg := Config{}.withDefaults()
	stale := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: cfg.HostIface},
		PeerName:  cfg.PeerIface,
	}
	require.NoError(t, netlink.LinkAdd(stale), "set up a link left by a dead container")
	t.Cleanup(func() {
		if l, err := netlink.LinkByName(cfg.HostIface); err == nil {
			_ = netlink.LinkDel(l)
		}
	})

	n, err := Provision(context.Background(), Config{ReclaimStaleLink: true}, slog.New(slog.DiscardHandler))
	require.NoError(t, err, "a restarted container must be able to provision again")
	t.Cleanup(func() { _ = n.Close() })
}
