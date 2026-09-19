package plexnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

const (
	egressChain = "plex-egress"
	egressSet   = "blocked"
)

// NFTBlocklist drops traffic from Plex to a set of addresses.
//
// The rule lives in the pod namespace on the forward path rather than inside
// Plex's namespace, so Plex cannot see or remove it, and so the set is managed
// from the process that knows who holds the lease.
type NFTBlocklist struct {
	// Subnet is the link Plex sits on; only traffic from it is filtered.
	Subnet netip.Prefix
	// NFTable is the nftables table to put the chain in.
	NFTable string
}

// Blocklist returns the filter for this network, for an EgressGuard to drive.
//
// It operates in the pod's own namespace, which is where Plex's traffic is
// forwarded and where the manager already runs, so it needs no handle.
func (n *Network) Blocklist() Blocklist {
	return &NFTBlocklist{Subnet: n.cfg.Subnet, NFTable: n.cfg.NFTable}
}

// Set replaces the blocked addresses. An empty set removes the filter entirely
// rather than leaving an empty one in place, so a pod that holds the lease has
// no rule at all.
func (b *NFTBlocklist) Set(_ context.Context, addrs []netip.Addr) error {
	return onLockedThread(func() error {
		conn, err := nftables.New()
		if err != nil {
			return fmt.Errorf("open nftables: %w", err)
		}

		table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: b.NFTable}
		chain := &nftables.Chain{
			Name:     egressChain,
			Table:    table,
			Type:     nftables.ChainTypeFilter,
			Hooknum:  nftables.ChainHookForward,
			Priority: nftables.ChainPriorityFilter,
		}
		// Rebuild rather than reconcile: the set is small and replacing it
		// wholesale avoids tracking what changed.
		//
		// The teardown gets its own flush. Sharing one with the rebuild means
		// the first install -- where there is no chain to tear down yet --
		// fails the whole batch on the delete.
		conn.DelChain(chain)
		if err := ignoreMissing(conn.Flush()); err != nil {
			return fmt.Errorf("remove egress filter: %w", err)
		}
		if len(addrs) == 0 {
			return nil
		}

		conn.AddChain(chain)
		set := &nftables.Set{Table: table, Name: egressSet, KeyType: nftables.TypeIPAddr}
		elements := make([]nftables.SetElement, 0, len(addrs))
		for _, a := range addrs {
			if !a.Is4() {
				continue
			}
			v := a.As4()
			elements = append(elements, nftables.SetElement{Key: v[:]})
		}
		if err := conn.AddSet(set, elements); err != nil {
			return fmt.Errorf("build blocked set: %w", err)
		}
		conn.AddRule(&nftables.Rule{
			Table: table,
			Chain: chain,
			Exprs: blockExprs(b.Subnet, set),
		})
		if err := conn.Flush(); err != nil {
			return fmt.Errorf("install egress filter: %w", err)
		}
		return nil
	})
}

// ignoreMissing swallows "it was not there" on a removal, and nothing else.
//
// Anything else has to surface: a removal that failed for another reason means
// the rule is still installed and we do not know it, so a pod would believe it
// had given up its route to plex.tv while keeping it.
func ignoreMissing(err error) error {
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// blockExprs drops packets from Plex's subnet whose destination is in the set.
func blockExprs(subnet netip.Prefix, set *nftables.Set) []expr.Any {
	network := subnet.Masked().Addr().As4()
	mask := net.CIDRMask(subnet.Bits(), 32)

	return []expr.Any{
		// Source must be on Plex's link, so nothing else in the pod is caught.
		&expr.Payload{
			DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4,
		},
		&expr.Bitwise{
			SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: mask, Xor: []byte{0, 0, 0, 0},
		},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: network[:]},

		// Destination in the blocked set.
		&expr.Payload{
			DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4,
		},
		&expr.Lookup{SourceRegister: 1, SetName: set.Name, SetID: set.ID},

		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}
