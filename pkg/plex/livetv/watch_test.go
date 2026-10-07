package livetv_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/mediactl/clusterplex/pkg/plex/livetv"
)

func TestProviderOfReadsTheAddressAndTheBlocks(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "clustarr.io/v1alpha1", "kind": "IPTVProvider",
		"metadata": map[string]any{"name": "polutv", "namespace": ns, "uid": "uid-1"},
		"spec":     map[string]any{"enabled": false},
		"status": map[string]any{
			"address":    "livetv.clustarr-system.svc:80",
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
			"blocks": []any{
				map[string]any{
					"start": "1000", "deviceID": "4e624e7a", "port": int64(47000), "friendlyName": "polutv 1000+",
					"guidePath": "/xmltv.xml", "lineupHash": "l1", "guideHash": "g1", "channels": int64(300),
				},
				map[string]any{"start": "1400", "deviceID": "0A1B2C3D", "port": int64(47001)},
			},
		},
	}}
	assert.Equal(t, livetv.Provider{
		Namespace: ns, Name: "polutv", UID: "uid-1", Ready: true, Enabled: false, EPGSource: "XEPG",
		Address: "livetv.clustarr-system.svc:80",
		Blocks: []livetv.Block{
			{Start: "1000", DeviceID: "4E624E7A", FriendlyName: "polutv 1000+", GuidePath: "/xmltv.xml", LineupHash: "l1", GuideHash: "g1", Port: 47000},
			{Start: "1400", DeviceID: "0A1B2C3D", Port: 47001},
		},
	}, livetv.ProviderOf(u))
}
