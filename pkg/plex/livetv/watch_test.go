package livetv_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

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
					"endpoint": "/livetv/clustarr-system/polutv/devices/4E624E7A",
				},
				map[string]any{"start": "1400", "deviceID": "0A1B2C3D", "port": int64(47001)},
			},
		},
	}}
	assert.Equal(t, livetv.Provider{
		Namespace: ns, Name: "polutv", UID: "uid-1", Ready: true, Enabled: false, EPGSource: "XEPG",
		Address: "livetv.clustarr-system.svc:80",
		Blocks: []livetv.Block{
			{
				Start: "1000", DeviceID: "4E624E7A", FriendlyName: "polutv 1000+", GuidePath: "/xmltv.xml", LineupHash: "l1", GuideHash: "g1",
				Endpoint: "/livetv/clustarr-system/polutv/devices/4E624E7A", Port: 47000,
			},
			{Start: "1400", DeviceID: "0A1B2C3D", Port: 47001},
		},
	}, livetv.ProviderOf(u))
}

func TestAMissingCRDNeverReportsAndEndsWithItsContext(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	called := make(chan struct{}, 1)
	w := &livetv.Watcher{
		Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), Discovery: cs.Discovery(), Namespace: ns,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Retry: 10 * time.Millisecond,
		OnChange: func([]livetv.Provider) { called <- struct{}{} },
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	require.NoError(t, w.Run(ctx), "it waits for the CRD, returning when its context ends")
	assert.Empty(t, called)
}

func TestTheWatcherReportsTheProvidersAsTheyComeAndGo(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	cs.Resources = []*metav1.APIResourceList{{GroupVersion: "clustarr.io/v1alpha1", APIResources: []metav1.APIResource{{Name: "iptvproviders", Namespaced: true, Kind: "IPTVProvider"}}}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "clustarr.io/v1alpha1", "kind": "IPTVProvider",
		"metadata": map[string]any{"name": "polutv", "namespace": ns},
		"status":   map[string]any{"address": "livetv.clustarr-system.svc:80", "blocks": []any{map[string]any{"start": "1000", "deviceID": "4E624E7A", "port": int64(47000)}}},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{livetv.GVR: "IPTVProviderList"}, obj)
	got := make(chan []livetv.Provider, 8)
	w := &livetv.Watcher{
		Dynamic: dyn, Discovery: cs.Discovery(), Namespace: ns, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Debounce: 10 * time.Millisecond, OnChange: func(live []livetv.Provider) { got <- live },
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	select {
	case live := <-got:
		require.Len(t, live, 1)
		assert.Equal(t, "polutv", live[0].Name)
		assert.Equal(t, int32(47000), live[0].Blocks[0].Port)
	case <-time.After(5 * time.Second):
		t.Fatal("no first list")
	}
	require.NoError(t, dyn.Resource(livetv.GVR).Namespace(ns).Delete(ctx, "polutv", metav1.DeleteOptions{}))
	select {
	case live := <-got:
		assert.Empty(t, live, "a deleted provider is simply absent")
	case <-time.After(5 * time.Second):
		t.Fatal("no list after the delete")
	}
}
