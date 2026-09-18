package plexnet

import (
	"net/netip"
	"testing"

	"github.com/google/nftables/expr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The kernel compares interface names against a fixed-width buffer. A name
// that is not padded matches nothing, and does so quietly.
func TestInterfaceNamesArePaddedToTheKernelsFixedWidth(t *testing.T) {
	got := ifname("plex0")

	require.Len(t, got, 16)
	assert.Equal(t, "plex0", string(got[:5]))
	assert.Equal(t, make([]byte, 11), got[5:], "the remainder must be NUL")
}

func TestMasqueradeMatchesTheLinkSubnetAndSkipsTrafficBackTowardsPlex(t *testing.T) {
	exprs := masqueradeExprs(netip.MustParsePrefix("169.254.1.0/30"), "plex0")

	// Source address of the IPv4 header: offset 12, four bytes.
	payload, ok := exprs[0].(*expr.Payload)
	require.True(t, ok, "first expression loads the source address")
	assert.Equal(t, expr.PayloadBaseNetworkHeader, payload.Base)
	assert.Equal(t, uint32(12), payload.Offset)
	assert.Equal(t, uint32(4), payload.Len)

	// Masking with the prefix length is what turns an address match into a
	// subnet match; a wrong mask matches one host or every host.
	bitwise, ok := exprs[1].(*expr.Bitwise)
	require.True(t, ok, "the address is masked to the prefix")
	assert.Equal(t, []byte{255, 255, 255, 252}, bitwise.Mask)
	assert.Equal(t, []byte{0, 0, 0, 0}, bitwise.Xor)

	network, ok := exprs[2].(*expr.Cmp)
	require.True(t, ok, "the masked address is compared to the network")
	assert.Equal(t, expr.CmpOpEq, network.Op)
	assert.Equal(t, []byte{169, 254, 1, 0}, network.Data)

	meta, ok := exprs[3].(*expr.Meta)
	require.True(t, ok, "the outbound interface is loaded")
	assert.Equal(t, expr.MetaKeyOIFNAME, meta.Key)

	// NEQ, not EQ: the rule masquerades everything leaving the pod *except*
	// what is going back over the link to Plex.
	iface, ok := exprs[4].(*expr.Cmp)
	require.True(t, ok, "the interface is compared")
	assert.Equal(t, expr.CmpOpNeq, iface.Op)
	assert.Equal(t, ifname("plex0"), iface.Data)

	_, ok = exprs[5].(*expr.Masq)
	assert.True(t, ok, "the rule ends by masquerading")
	assert.Len(t, exprs, 6)
}

func TestMasqueradeDerivesTheMaskFromThePrefixLength(t *testing.T) {
	tests := []struct {
		subnet string
		mask   []byte
		net    []byte
	}{
		{"169.254.1.0/30", []byte{255, 255, 255, 252}, []byte{169, 254, 1, 0}},
		{"10.255.0.0/24", []byte{255, 255, 255, 0}, []byte{10, 255, 0, 0}},
		{"192.168.99.4/30", []byte{255, 255, 255, 252}, []byte{192, 168, 99, 4}},
	}
	for _, tt := range tests {
		t.Run(tt.subnet, func(t *testing.T) {
			exprs := masqueradeExprs(netip.MustParsePrefix(tt.subnet), "plex0")
			assert.Equal(t, tt.mask, exprs[1].(*expr.Bitwise).Mask)
			assert.Equal(t, tt.net, exprs[2].(*expr.Cmp).Data)
		})
	}
}
