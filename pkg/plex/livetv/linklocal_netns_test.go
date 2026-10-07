//go:build netns

// Run by `make test-netns`, inside a fresh network namespace: adding an
// address to lo needs CAP_NET_ADMIN.
package livetv_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	"github.com/mediactl/clusterplex/pkg/plex/livetv"
)

func TestTheLinkLocalAddressIsAddedToLoOnceAndServesTheBlocks(t *testing.T) {
	lo, err := netlink.LinkByName("lo")
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetUp(lo))

	require.NoError(t, livetv.EnsureLinkLocal(livetv.DefaultLocalAddress))
	require.NoError(t, livetv.EnsureLinkLocal(livetv.DefaultLocalAddress), "an address already there is success")
	addrs, err := netlink.AddrList(lo, netlink.FAMILY_V4)
	require.NoError(t, err)
	var got []string
	for _, a := range addrs {
		got = append(got, a.IPNet.String())
	}
	assert.Contains(t, got, "169.254.47.1/32")

	tuner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.Host) }))
	defer tuner.Close()
	ps := &livetv.ProxySet{LocalAddress: livetv.DefaultLocalAddress}
	defer ps.Close()
	require.NoError(t, ps.Apply([]livetv.Provider{{Address: strings.TrimPrefix(tuner.URL, "http://"), Blocks: []livetv.Block{{Start: "1000", Port: 47000}}}}))
	resp, err := http.Get("http://169.254.47.1:47000/discover.json")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	assert.Equal(t, net.JoinHostPort("169.254.47.1", "47000"), string(b))

	require.Error(t, livetv.EnsureLinkLocal("fe80::1"), "IPv4 only")
}
