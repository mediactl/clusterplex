package plexnet

import (
	"fmt"
	"net/netip"
)

// Defaults for Config. The subnet is link-local because the link is
// point-to-point and its addresses are never seen outside the pod.
const (
	defaultHostIface = "plex0"
	defaultPeerIface = "plex1"
	defaultSubnet    = "169.254.1.0/30"
	defaultPlexPort  = 32400
	defaultNFTable   = "clusterplex"

	// maxIfaceName is IFNAMSIZ-1. The kernel rejects anything longer, a long
	// way from where the name was chosen.
	maxIfaceName = 15
)

// Config describes the isolated network given to Plex Media Server.
//
// The zero value is usable: withDefaults fills every field, so a caller that
// wants the standard layout passes Config{}.
type Config struct {
	// HostIface is the pod-side end of the veth pair.
	HostIface string
	// PeerIface is the end that is moved into Plex's namespace.
	PeerIface string
	// Subnet is the point-to-point link joining the two namespaces. The first
	// usable address is the pod-side gateway and the second is Plex.
	Subnet netip.Prefix
	// PlexPort is the port Plex binds inside its own namespace.
	PlexPort int
	// NFTable names the nftables table holding the masquerade rule. Teardown
	// deletes the table by name, so it must be ours alone.
	NFTable string
}

func (c Config) withDefaults() Config {
	if c.HostIface == "" {
		c.HostIface = defaultHostIface
	}
	if c.PeerIface == "" {
		c.PeerIface = defaultPeerIface
	}
	if !c.Subnet.IsValid() {
		c.Subnet = netip.MustParsePrefix(defaultSubnet)
	}
	if c.PlexPort == 0 {
		c.PlexPort = defaultPlexPort
	}
	if c.NFTable == "" {
		c.NFTable = defaultNFTable
	}
	return c
}

// Validate reports configuration that cannot produce a working network. It is
// called on a defaulted Config, so every field is set.
func (c Config) Validate() error {
	if !c.Subnet.IsValid() {
		return fmt.Errorf("subnet %q is not a valid prefix", c.Subnet)
	}
	// IPv6 is deliberately out of scope; addressing only half the link would
	// be worse than refusing.
	if !c.Subnet.Addr().Is4() {
		return fmt.Errorf("subnet %s must be IPv4", c.Subnet)
	}
	if c.Subnet.Bits() > 30 {
		return fmt.Errorf("subnet %s must hold at least two addresses, so /30 or wider", c.Subnet)
	}
	if c.HostIface == c.PeerIface {
		return fmt.Errorf("host and peer interface names must differ, both are %q", c.HostIface)
	}
	for _, name := range []string{c.HostIface, c.PeerIface} {
		if len(name) > maxIfaceName {
			return fmt.Errorf("interface name %q is longer than the kernel's limit of %d characters", name, maxIfaceName)
		}
	}
	if c.PlexPort < 1 || c.PlexPort > 65535 {
		return fmt.Errorf("plex port %d is out of range", c.PlexPort)
	}
	return nil
}

// GatewayAddr is the pod-side address, and the next hop for everything Plex
// sends.
func (c Config) GatewayAddr() netip.Addr { return nthAddr(c.Subnet, 1) }

// PlexAddr is the address Plex is reachable on from the pod namespace.
func (c Config) PlexAddr() netip.Addr { return nthAddr(c.Subnet, 2) }

// PlexAddrPort is where the proxy dials Plex.
func (c Config) PlexAddrPort() netip.AddrPort {
	return netip.AddrPortFrom(c.PlexAddr(), uint16(c.PlexPort))
}

// nthAddr returns the nth address after the base of the prefix, so nthAddr(p, 1)
// is the first usable host address.
func nthAddr(p netip.Prefix, n int) netip.Addr {
	a := p.Masked().Addr()
	for range n {
		a = a.Next()
	}
	return a
}
