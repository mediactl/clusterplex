//go:build netns

// These tests provision real network namespaces and need CAP_SYS_ADMIN. Run
// them with `make test-netns`, which re-execs the suite inside a user
// namespace so no actual root is required.
package net

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// readSysctl reads a namespaced sysctl through /proc, so the test observes
// what the kernel actually holds rather than what the code believes it wrote.
func readSysctl(t *testing.T, key string) string {
	t.Helper()
	b, err := os.ReadFile("/proc/sys/" + key)
	require.NoError(t, err)
	return string(b)
}

func provision(t *testing.T) *Network {
	t.Helper()
	n, err := Provision(context.Background(), Config{}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() { _ = n.Close() })
	return n
}

// currentNS reads the namespace of the calling thread. The caller must have
// locked its thread, or the answer is meaningless.
//
// It reads /proc/thread-self rather than using NsHandle.UniqueId, whose string
// embeds the file descriptor number: two handles on the same namespace compare
// unequal, and two on different namespaces can compare equal if the fd happens
// to be reused.
func currentNS(t *testing.T) string {
	t.Helper()
	l, err := os.Readlink("/proc/thread-self/ns/net")
	require.NoError(t, err)
	return l
}

// nsOf reports the namespace a handle refers to, in the same spelling
// currentNS uses.
func nsOf(t *testing.T, n *Network) string {
	t.Helper()
	var seen string
	require.NoError(t, n.Do(func() error {
		l, err := os.Readlink("/proc/thread-self/ns/net")
		seen = l
		return err
	}))
	return seen
}

// This is the property everything else depends on. A thread left in Plex's
// namespace goes back to the scheduler, and the next goroutine to land on it
// silently gets Plex's network instead of the pod's.
func TestProvisioningLeavesTheCallersNamespaceAlone(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	before := currentNS(t)
	n := provision(t)
	assert.Equal(t, before, currentNS(t), "the calling thread must come back")

	// And the namespace it built is genuinely a different one.
	assert.NotEqual(t, before, nsOf(t, n))
}

func TestDoRestoresTheCallersNamespaceEvenWhenTheFunctionFails(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	n := provision(t)
	before := currentNS(t)

	err := n.Do(func() error { return assert.AnError })

	require.ErrorIs(t, err, assert.AnError, "the function's error reaches the caller")
	assert.Equal(t, before, currentNS(t))
}

func TestDoRunsTheFunctionInsideThePlexNamespace(t *testing.T) {
	n := provision(t)

	var inside bool
	require.NoError(t, n.Do(func() error {
		h, err := netns.Get()
		if err != nil {
			return err
		}
		defer func() { _ = h.Close() }()
		inside = h.Equal(n.handle)
		return nil
	}))

	assert.True(t, inside, "Do must run fn in the network it provisioned")
}

func TestPlexSideOfTheLinkIsAddressedAndRoutedThroughThePod(t *testing.T) {
	n := provision(t)
	cfg := Config{}.withDefaults()

	var (
		loUp   bool
		addrs  []netlink.Addr
		routes []netlink.Route
	)
	require.NoError(t, n.Do(func() error {
		// Plex needs localhost for its own helper traffic.
		lo, err := netlink.LinkByName("lo")
		if err != nil {
			return err
		}
		loUp = lo.Attrs().Flags&net.FlagUp != 0

		peer, err := netlink.LinkByName(cfg.PeerIface)
		if err != nil {
			return err
		}
		if addrs, err = netlink.AddrList(peer, netlink.FAMILY_V4); err != nil {
			return err
		}
		routes, err = netlink.RouteList(nil, netlink.FAMILY_V4)
		return err
	}))

	assert.True(t, loUp, "loopback must be up")
	require.Len(t, addrs, 1)
	assert.Equal(t, cfg.PlexAddr().String(), addrs[0].IP.String())

	var gw string
	for _, r := range routes {
		if r.Dst == nil || r.Dst.String() == "0.0.0.0/0" {
			if r.Gw != nil {
				gw = r.Gw.String()
			}
		}
	}
	assert.Equal(t, cfg.GatewayAddr().String(), gw, "default route via the pod side; routes: %+v", routes)
}

func TestPodSideOfTheLinkIsAddressedAndUp(t *testing.T) {
	provision(t)
	cfg := Config{}.withDefaults()

	host, err := netlink.LinkByName(cfg.HostIface)
	require.NoError(t, err)
	assert.NotZero(t, host.Attrs().Flags&1, "host side must be up")

	addrs, err := netlink.AddrList(host, netlink.FAMILY_V4)
	require.NoError(t, err)
	require.Len(t, addrs, 1)
	assert.Equal(t, cfg.GatewayAddr().String(), addrs[0].IP.String())
}

// The whole point: Plex's process must land in the namespace, not merely the
// goroutine that started it.
func TestStartProcessPutsTheChildInThePlexNamespace(t *testing.T) {
	n := provision(t)

	var out strings.Builder
	cmd := exec.Command("readlink", "/proc/self/ns/net")
	cmd.Stdout = &out
	require.NoError(t, n.StartProcess(cmd))
	require.NoError(t, cmd.Wait())

	assert.Equal(t, nsOf(t, n), strings.TrimSpace(out.String()))
}

func TestStartProcessLeavesTheCallersNamespaceAlone(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	n := provision(t)
	before := currentNS(t)

	cmd := exec.Command("true")
	require.NoError(t, n.StartProcess(cmd))
	require.NoError(t, cmd.Wait())

	assert.Equal(t, before, currentNS(t))
}

// Forwarding is what lets a packet cross from the link to the pod's uplink.
// Without it the masquerade rule is never reached.
func TestForwardingIsEnabledSoTheMasqueradeIsReachable(t *testing.T) {
	provision(t)
	assert.Equal(t, "1", strings.TrimSpace(readSysctl(t, "net/ipv4/ip_forward")))
}

func TestCloseRemovesThePodSideOfTheLink(t *testing.T) {
	cfg := Config{}.withDefaults()
	n, err := Provision(context.Background(), Config{}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	_, err = netlink.LinkByName(cfg.HostIface)
	require.NoError(t, err, "the link exists while the network does")

	require.NoError(t, n.Close())

	_, err = netlink.LinkByName(cfg.HostIface)
	assert.Error(t, err, "Close must not leave the veth behind")
}

// A second Provision must not half-build a network on top of the first and
// leave the pod with a stray interface.
func TestProvisioningTwiceFailsCleanlyRatherThanLeavingDebris(t *testing.T) {
	provision(t)

	second, err := Provision(context.Background(), Config{}, slog.New(slog.DiscardHandler))
	if err == nil {
		_ = second.Close()
		t.Fatal("expected the second Provision to fail: the interface name is taken")
	}

	host := Config{}.withDefaults().HostIface
	links, lerr := netlink.LinkList()
	require.NoError(t, lerr)
	count := 0
	for _, l := range links {
		if l.Attrs().Name == host {
			count++
		}
	}
	assert.Equal(t, 1, count, "only the first network's link may remain")
}

func TestBlockingAddressesWorksTheFirstTimeThereIsNoFilterYet(t *testing.T) {
	// The chain is torn down and rebuilt on every change, and the teardown
	// used to share a flush with the rebuild. On the very first install there
	// is no chain to tear down, so the whole batch failed:
	//
	//	install egress filter: conn.Receive: netlink receive:
	//	  no such file or directory
	//
	// Nothing caught it, because the lease holder is the only pod that ever
	// had an empty set and every other pod started from one it had installed
	// itself. Blocking the lease holder too -- which is how a cluster runs
	// while Plex cannot survive talking to plex.tv -- goes straight down this
	// path.
	net := provision(t)
	block := net.Blocklist()

	err := net.Do(func() error {
		return block.Set(context.Background(), []netip.Addr{
			netip.MustParseAddr("198.51.100.7"),
		})
	})
	require.NoError(t, err, "the first install has no chain to replace")

	// And again, now that there is one to replace.
	err = net.Do(func() error {
		return block.Set(context.Background(), []netip.Addr{
			netip.MustParseAddr("198.51.100.8"),
		})
	})
	require.NoError(t, err)

	// Removing it is still fine.
	err = net.Do(func() error {
		return block.Set(context.Background(), nil)
	})
	require.NoError(t, err)
}
