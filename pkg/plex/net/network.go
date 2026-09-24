// Package net gives Plex Media Server a network namespace of its own,
// joined to the pod by a veth pair.
//
// Plex always binds 0.0.0.0:32400, and 32401 beside it, with no setting to
// change either. Isolating it frees those ports in the pod namespace, so the
// manager's proxy can take 32400 itself and no packet can reach Plex without
// passing through it. See docs/adr/0003.
package net

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// Network is the isolated network Plex runs in. It is provisioned once and
// held for the life of the manager process; a container restart gets a fresh
// one rather than reconciling whatever the last one left behind.
type Network struct {
	cfg    Config
	handle netns.NsHandle
	log    *slog.Logger
}

// Provision builds the namespace, the veth pair joining it to the pod, the
// addresses and routes inside it, and the masquerade that lets Plex out.
//
// It fails rather than half-succeeding: anything it created is removed before
// it returns an error, so a retry does not trip over its own debris.
func Provision(_ context.Context, cfg Config, log *slog.Logger) (*Network, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("plexnet configuration: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}

	var handle netns.NsHandle
	if err := onLockedThread(func() error {
		var err error
		handle, err = build(cfg, log)
		return err
	}); err != nil {
		return nil, err
	}

	log.Info("provisioned Plex network namespace",
		"namespace", handle.UniqueId(),
		"host", cfg.HostIface,
		"peer", cfg.PeerIface,
		"plex", cfg.PlexAddrPort().String(),
		"gateway", cfg.GatewayAddr().String())
	return &Network{cfg: cfg, handle: handle, log: log}, nil
}

// Do runs fn on a thread inside Plex's namespace, and returns that thread to
// the caller's namespace afterwards.
func (n *Network) Do(fn func() error) error {
	return onLockedThread(func() error {
		if err := netns.Set(n.handle); err != nil {
			return fmt.Errorf("enter Plex network namespace: %w", err)
		}
		return fn()
	})
}

// StartProcess starts cmd inside Plex's namespace.
//
// The child inherits the namespace because fork copies the calling thread's
// namespaces, and os/exec forks from the calling goroutine's thread — which
// Do has locked and switched. That chain is what makes this work; none of it
// is visible in the code below.
//
// Note that cmd must not set SysProcAttr.Pdeathsig. That signal fires when the
// *thread* that forked exits, and the thread here is a temporary one that the
// Go runtime is free to retire at any point afterwards, which would kill Plex
// for no reason. The supervisor manages Plex's lifetime instead.
func (n *Network) StartProcess(cmd *exec.Cmd) error {
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Pdeathsig != 0 {
		return errors.New("SysProcAttr.Pdeathsig cannot be used with a namespaced start: it would fire when the forking thread is retired")
	}
	return n.Do(func() error { return cmd.Start() })
}

// PlexAddrPort is where the proxy dials Plex.
func (n *Network) PlexAddrPort() netip.AddrPort { return n.cfg.PlexAddrPort() }

// Close removes the veth pair and the nftables table, and releases the
// namespace, which the kernel reaps once nothing references it.
func (n *Network) Close() error {
	if n == nil || n.handle == 0 {
		return nil
	}
	err := onLockedThread(func() error {
		// Close runs in the pod namespace, which is where both the veth and
		// the nftables table live, so nothing is switched here.
		pod, err := netns.Get()
		if err != nil {
			return fmt.Errorf("read current network namespace: %w", err)
		}
		defer func() { _ = pod.Close() }()
		return errors.Join(deleteLink(n.cfg.HostIface), removeMasquerade(pod, n.cfg))
	})
	return errors.Join(err, n.handle.Close())
}

// build does the whole provisioning sequence. It runs on a locked thread whose
// namespace onLockedThread guarantees to restore, which is what lets it switch
// namespaces freely and still return early on any error.
func build(cfg Config, log *slog.Logger) (netns.NsHandle, error) {
	pod, err := netns.Get()
	if err != nil {
		return 0, fmt.Errorf("read pod network namespace: %w", err)
	}
	defer func() { _ = pod.Close() }()

	// netns.New switches this thread into the namespace it creates; the veth
	// pair has to be built in the pod namespace, so come straight back.
	plexNS, err := netns.New()
	if err != nil {
		return 0, fmt.Errorf("create Plex network namespace: %w", err)
	}
	if err := netns.Set(pod); err != nil {
		_ = plexNS.Close()
		return 0, fmt.Errorf("return to pod network namespace: %w", err)
	}

	// Undo only what this call created. Tearing down by name instead would
	// let a second, failing Provision delete the veth and nftables table
	// belonging to the first one — which is how a retry takes the working
	// network down with it.
	done := false
	var undo []func()
	defer func() {
		if done {
			return
		}
		// Tear down in the pod namespace: a failure inside the Plex namespace
		// would otherwise have us deleting links in the wrong place.
		_ = netns.Set(pod)
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		_ = plexNS.Close()
	}()

	// A link already holding our name is one this pod left behind. The name is
	// fixed, the pod's network namespace belongs to this pod alone, and that
	// namespace outlives the container inside it — so a container that
	// restarts finds its predecessor's veth still there and LinkAdd refuses
	// with EEXIST. Provisioning is fatal, so the pod then crash-loops for
	// ever: one restart for any reason and it never comes back.
	//
	// Removing it is not inheriting it. Whatever state it is in, it is wired
	// to a Plex namespace that died with the container, not to the one being
	// created here.
	if stale, err := netlink.LinkByName(cfg.HostIface); err == nil && cfg.ReclaimStaleLink {
		log.Info("removing the veth a previous container left behind", "link", cfg.HostIface)
		if err := netlink.LinkDel(stale); err != nil {
			return 0, fmt.Errorf("remove stale veth %s: %w", cfg.HostIface, err)
		}
	}

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: cfg.HostIface},
		PeerName:  cfg.PeerIface,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return 0, fmt.Errorf("create veth pair %s/%s: %w", cfg.HostIface, cfg.PeerIface, err)
	}
	undo = append(undo, func() { _ = deleteLink(cfg.HostIface) })

	host, err := netlink.LinkByName(cfg.HostIface)
	if err != nil {
		return 0, fmt.Errorf("find %s: %w", cfg.HostIface, err)
	}
	peer, err := netlink.LinkByName(cfg.PeerIface)
	if err != nil {
		return 0, fmt.Errorf("find %s: %w", cfg.PeerIface, err)
	}

	if err := addAddr(host, cfg.GatewayAddr(), cfg.Subnet.Bits()); err != nil {
		return 0, err
	}
	if err := netlink.LinkSetUp(host); err != nil {
		return 0, fmt.Errorf("bring up %s: %w", cfg.HostIface, err)
	}
	if err := netlink.LinkSetNsFd(peer, int(plexNS)); err != nil {
		return 0, fmt.Errorf("move %s into the Plex namespace: %w", cfg.PeerIface, err)
	}

	// Forwarding and the masquerade are both pod-namespace settings: they are
	// what turns the link from a dead end into a route out.
	if err := enableForwarding(); err != nil {
		return 0, err
	}
	if err := installMasquerade(pod, cfg); err != nil {
		return 0, err
	}
	undo = append(undo, func() { _ = removeMasquerade(pod, cfg) })

	if err := netns.Set(plexNS); err != nil {
		return 0, fmt.Errorf("enter Plex network namespace: %w", err)
	}
	if err := configurePlexSide(cfg); err != nil {
		return 0, err
	}
	if err := netns.Set(pod); err != nil {
		return 0, fmt.Errorf("return to pod network namespace: %w", err)
	}

	done = true
	return plexNS, nil
}

// configurePlexSide runs inside the Plex namespace.
func configurePlexSide(cfg Config) error {
	// Plex reaches its own helpers over localhost, and a namespace starts with
	// loopback down.
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("find loopback in the Plex namespace: %w", err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		return fmt.Errorf("bring up loopback in the Plex namespace: %w", err)
	}

	peer, err := netlink.LinkByName(cfg.PeerIface)
	if err != nil {
		return fmt.Errorf("find %s in the Plex namespace: %w", cfg.PeerIface, err)
	}
	if err := addAddr(peer, cfg.PlexAddr(), cfg.Subnet.Bits()); err != nil {
		return err
	}
	// The route below needs the link up first.
	if err := netlink.LinkSetUp(peer); err != nil {
		return fmt.Errorf("bring up %s: %w", cfg.PeerIface, err)
	}

	gw := cfg.GatewayAddr()
	route := &netlink.Route{
		LinkIndex: peer.Attrs().Index,
		Scope:     netlink.SCOPE_UNIVERSE,
		Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		Gw:        net.IP(gw.AsSlice()),
	}
	if err := netlink.RouteAdd(route); err != nil {
		return fmt.Errorf("add default route via %s: %w", gw, err)
	}
	return nil
}

// onLockedThread runs fn on a dedicated OS thread and guarantees that thread
// goes back to the namespace it started in, or never runs anything again.
//
// This is the only place that moves a thread between namespaces. Getting it
// wrong is not a local failure: an unlocked thread left in Plex's namespace
// returns to the scheduler, and the next goroutine to land on it silently
// inherits Plex's network. A deferred restore is not enough, because the case
// that matters is the restore itself failing — so on that path the goroutine
// returns while still locked, and the Go runtime destroys the thread.
func onLockedThread(fn func() error) error {
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()

		origin, err := netns.Get()
		if err != nil {
			runtime.UnlockOSThread()
			result <- fmt.Errorf("read current network namespace: %w", err)
			return
		}

		ferr := fn()

		if serr := netns.Set(origin); serr != nil {
			_ = origin.Close()
			// Deliberately not unlocking: this thread is in an unknown
			// namespace and must never be reused.
			result <- errors.Join(ferr, fmt.Errorf("restore network namespace, thread retired: %w", serr))
			return
		}
		_ = origin.Close()
		runtime.UnlockOSThread()
		result <- ferr
	}()
	return <-result
}

func addAddr(link netlink.Link, addr netip.Addr, bits int) error {
	a := &netlink.Addr{IPNet: &net.IPNet{
		IP:   net.IP(addr.AsSlice()),
		Mask: net.CIDRMask(bits, 32),
	}}
	if err := netlink.AddrAdd(link, a); err != nil {
		return fmt.Errorf("address %s with %s: %w", link.Attrs().Name, a, err)
	}
	return nil
}

// deleteLink removes a link if it is there. Deleting one end of a veth pair
// takes the other with it.
func deleteLink(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		var missing netlink.LinkNotFoundError
		if errors.As(err, &missing) {
			return nil
		}
		return fmt.Errorf("find %s: %w", name, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("delete %s: %w", name, err)
	}
	return nil
}

// ipForwardPath is the pod namespace's IPv4 forwarding switch.
const ipForwardPath = "/proc/sys/net/ipv4/ip_forward"

// enableForwarding turns on IPv4 forwarding in the current namespace. Without
// it a packet from Plex is dropped before it ever reaches the masquerade.
func enableForwarding() error { return enableForwardingAt(ipForwardPath) }

// enableForwardingAt is enableForwarding on a given sysctl file.
//
// It reads before it writes. A pod that is not privileged has /proc/sys
// mounted read-only, and the write fails whatever the value -- but the value
// can already be right, because Kubernetes sets net.ipv4.ip_forward=1 in the
// pod namespace before any container starts when the pod's securityContext
// asks for it (sysctls: net.ipv4.ip_forward). Forwarding that is already on
// is what we want, however it got there; only forwarding that is off and
// cannot be turned on is an error, and then the message says what to do.
func enableForwardingAt(path string) error {
	if current, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(current)) == "1" {
		return nil
	}
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable IPv4 forwarding: %w (an unprivileged pod needs net.ipv4.ip_forward=1 "+
			"in its securityContext.sysctls, which the kubelet must allow-list)", err)
	}
	return nil
}
