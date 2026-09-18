package plexnet

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnEmptyConfigIsUsable(t *testing.T) {
	c := Config{}.withDefaults()

	require.NoError(t, c.Validate())
	assert.Equal(t, "plex0", c.HostIface)
	assert.Equal(t, "plex1", c.PeerIface)
	assert.Equal(t, netip.MustParsePrefix("169.254.1.0/30"), c.Subnet)
	assert.Equal(t, 32400, c.PlexPort)
	assert.Equal(t, "clusterplex", c.NFTable)
}

func TestDefaultingLeavesSuppliedValuesAlone(t *testing.T) {
	c := Config{
		HostIface: "veth-host",
		PeerIface: "veth-plex",
		Subnet:    netip.MustParsePrefix("10.255.0.0/30"),
		PlexPort:  1234,
		NFTable:   "custom",
	}.withDefaults()

	assert.Equal(t, "veth-host", c.HostIface)
	assert.Equal(t, "veth-plex", c.PeerIface)
	assert.Equal(t, netip.MustParsePrefix("10.255.0.0/30"), c.Subnet)
	assert.Equal(t, 1234, c.PlexPort)
	assert.Equal(t, "custom", c.NFTable)
}

// The gateway is the pod side of the link and the one Plex routes through, so
// getting these two the wrong way round would be silent: both addresses exist.
func TestTheGatewayIsTheFirstHostAddressAndPlexIsTheSecond(t *testing.T) {
	tests := []struct {
		subnet  string
		gateway string
		plex    string
	}{
		{"169.254.1.0/30", "169.254.1.1", "169.254.1.2"},
		{"10.255.0.0/30", "10.255.0.1", "10.255.0.2"},
		{"192.168.99.4/30", "192.168.99.5", "192.168.99.6"},
		// A /24 is wasteful but legal; the first two usable addresses win.
		{"172.20.0.0/24", "172.20.0.1", "172.20.0.2"},
	}
	for _, tt := range tests {
		t.Run(tt.subnet, func(t *testing.T) {
			c := Config{Subnet: netip.MustParsePrefix(tt.subnet)}.withDefaults()
			assert.Equal(t, netip.MustParseAddr(tt.gateway), c.GatewayAddr())
			assert.Equal(t, netip.MustParseAddr(tt.plex), c.PlexAddr())
		})
	}
}

func TestPlexAddrPortCombinesTheAddressAndTheConfiguredPort(t *testing.T) {
	c := Config{}.withDefaults()
	assert.Equal(t, "169.254.1.2:32400", c.PlexAddrPort().String())
}

func TestValidateRejectsConfigurationThatCannotWork(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			// /31 leaves no room for a gateway and a peer.
			name: "subnet too small for two addresses",
			cfg:  Config{Subnet: netip.MustParsePrefix("169.254.1.0/31")},
			want: "at least two addresses",
		},
		{
			// IPv6 is deliberately out of scope; failing loudly beats
			// provisioning something half-addressed.
			name: "IPv6 subnet",
			cfg:  Config{Subnet: netip.MustParsePrefix("fd00::/64")},
			want: "IPv4",
		},
		{
			name: "interface names collide",
			cfg:  Config{HostIface: "same", PeerIface: "same"},
			want: "must differ",
		},
		{
			// IFNAMSIZ is 16 including the NUL, so 15 is the limit. The
			// kernel would reject it far from here.
			name: "interface name too long for the kernel",
			cfg:  Config{HostIface: "abcdefghijklmnop"},
			want: "15",
		},
		{
			name: "port out of range",
			cfg:  Config{PlexPort: 70000},
			want: "port",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.withDefaults().Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}
