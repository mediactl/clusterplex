package livetv

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"github.com/vishvananda/netlink"
)

// EnsureLinkLocal adds addr/32 to lo in the pod's network namespace, where
// the blocks' proxies listen. Plex, in its own namespace, reaches it through
// its default route to the pod. An address already there is success.
func EnsureLinkLocal(addr string) error {
	ip, err := netip.ParseAddr(addr)
	if err != nil || !ip.Is4() {
		return fmt.Errorf("livetv: %q is not an IPv4 address", addr)
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("livetv: find lo: %w", err)
	}
	a := &netlink.Addr{IPNet: &net.IPNet{IP: net.IP(ip.AsSlice()), Mask: net.CIDRMask(32, 32)}}
	if err := netlink.AddrAdd(lo, a); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("livetv: add %s/32 to lo: %w", addr, err)
	}
	return nil
}
