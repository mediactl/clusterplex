package net

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netns"
)

// ifnameLen is the fixed width the kernel compares interface names against.
// A name shorter than this is NUL-padded; an unpadded name matches nothing.
const ifnameLen = 16

// ifname encodes an interface name for comparison against meta oifname.
func ifname(s string) []byte {
	b := make([]byte, ifnameLen)
	copy(b, s)
	return b
}

// masqueradeExprs builds the rule body: masquerade traffic sourced from the
// link subnet, except what is leaving over the link itself.
//
// Without the interface test the rule would also rewrite replies travelling
// back towards Plex. That is almost invisible — the connection still works —
// and shows up much later as a packet capture that makes no sense.
func masqueradeExprs(subnet netip.Prefix, hostIface string) []expr.Any {
	network := subnet.Masked().Addr().As4()
	mask := net.CIDRMask(subnet.Bits(), 32)

	return []expr.Any{
		// Load the IPv4 source address: offset 12, four bytes.
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       12,
			Len:          4,
		},
		// Mask it down to the prefix, so this matches the subnet rather than
		// a single host.
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           mask,
			Xor:            []byte{0, 0, 0, 0},
		},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: network[:]},
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: ifname(hostIface)},
		&expr.Masq{},
	}
}

// installMasquerade creates the table, chain and rule that let Plex reach the
// outside world. Packets leave its namespace sourced from the link subnet,
// which nothing beyond the pod can route a reply to, so the pod rewrites them
// to its own address.
//
// It must run in the namespace ns, which owns the pod's egress interface.
func installMasquerade(ns netns.NsHandle, cfg Config) error {
	conn, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	if err != nil {
		return fmt.Errorf("open nftables connection: %w", err)
	}
	defer func() { _ = conn.CloseLasting() }()

	// Flushing our own table first makes this idempotent: a second call
	// replaces the rule instead of adding a duplicate.
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: cfg.NFTable}
	conn.AddTable(table)
	conn.FlushTable(table)

	conn.AddChain(&nftables.Chain{
		Name:     postroutingChain,
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
	})
	conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: &nftables.Chain{Name: postroutingChain, Table: table},
		Exprs: masqueradeExprs(cfg.Subnet, cfg.HostIface),
	})

	if err := conn.Flush(); err != nil {
		return fmt.Errorf("install masquerade for %s: %w", cfg.Subnet, err)
	}
	return nil
}

// removeMasquerade deletes the table installed by installMasquerade. The table
// is ours alone, which is why deleting it wholesale is safe.
func removeMasquerade(ns netns.NsHandle, cfg Config) error {
	conn, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	if err != nil {
		return fmt.Errorf("open nftables connection: %w", err)
	}
	defer func() { _ = conn.CloseLasting() }()

	conn.DelTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: cfg.NFTable})
	if err := conn.Flush(); err != nil {
		return fmt.Errorf("remove nftables table %q: %w", cfg.NFTable, err)
	}
	return nil
}

const postroutingChain = "postrouting"
