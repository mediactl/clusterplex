package livetv

import (
	"context"
	"log/slog"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// GVR is clustarr's IPTVProvider, read as unstructured: cluster-plex imports
// none of clustarr's code.
var GVR = schema.GroupVersionResource{Group: "clustarr.io", Version: "v1alpha1", Resource: "iptvproviders"}

// Watcher follows the IPTVProviders of one namespace. It is its own
// informer, apart from pkg/clustarrwatch, so a cluster without the CRD
// never stalls the catalog watch: it logs once and checks again.
type Watcher struct {
	Dynamic   dynamic.Interface
	Discovery discovery.DiscoveryInterface
	Namespace string
	Logger    *slog.Logger
	// OnChange is told the providers after Debounce of quiet. A deleted
	// provider is simply absent: its blocks' devices are on the link-local
	// address, which is how the converger finds them.
	OnChange func(live []Provider)
	// Retry is how often a missing CRD is looked for again. Default 5m.
	Retry time.Duration
	// Debounce gathers a burst of changes. Default 2s.
	Debounce time.Duration
}

// Run watches until ctx ends.
func (w *Watcher) Run(ctx context.Context) error {
	retry, debounce := w.Retry, w.Debounce
	if retry <= 0 {
		retry = 5 * time.Minute
	}
	if debounce <= 0 {
		debounce = 2 * time.Second
	}
	for logged := false; !w.installed(); {
		if !logged {
			w.Logger.Info("iptvproviders.clustarr.io is not installed; Live TV waits for it", "retry", retry)
			logged = true
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}

	f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(w.Dynamic, 0, w.Namespace, nil)
	inf := f.ForResource(GVR).Informer()
	poke := make(chan struct{}, 1)
	wake := func() {
		select {
		case poke <- struct{}{}:
		default:
		}
	}
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { wake() },
		UpdateFunc: func(any, any) { wake() },
		DeleteFunc: func(any) { wake() },
	}); err != nil {
		return err
	}
	f.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
		return nil
	}
	wake()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-poke:
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(debounce):
		}
		var live []Provider
		for _, o := range inf.GetStore().List() {
			if u, ok := o.(*unstructured.Unstructured); ok {
				live = append(live, ProviderOf(u))
			}
		}
		w.OnChange(live)
	}
}

func (w *Watcher) installed() bool {
	rl, err := w.Discovery.ServerResourcesForGroupVersion(GVR.GroupVersion().String())
	if err != nil {
		if !apierrors.IsNotFound(err) {
			w.Logger.Warn("look for iptvproviders.clustarr.io", "error", err)
		}
		return false
	}
	for _, r := range rl.APIResources {
		if r.Name == GVR.Resource {
			return true
		}
	}
	return false
}

// ProviderOf reads the fields the converger needs from an IPTVProvider:
// status.address and status.blocks, as clustarr publishes them.
func ProviderOf(u *unstructured.Unstructured) Provider {
	p := Provider{
		Namespace: u.GetNamespace(), Name: u.GetName(), UID: string(u.GetUID()),
		Address:   nestedString(u.Object, "status", "address"),
		EPGSource: nestedString(u.Object, "spec", "epgSource"),
		Enabled:   true,
	}
	if v, found, _ := unstructured.NestedBool(u.Object, "spec", "enabled"); found {
		p.Enabled = v
	}
	if p.EPGSource == "" {
		p.EPGSource = "XEPG"
	}
	blocks, _, _ := unstructured.NestedSlice(u.Object, "status", "blocks")
	for _, o := range blocks {
		m, ok := o.(map[string]any)
		if !ok {
			continue
		}
		port, _, _ := unstructured.NestedInt64(m, "port")
		p.Blocks = append(p.Blocks, Block{
			Start:        nestedString(m, "start"),
			DeviceID:     strings.ToUpper(nestedString(m, "deviceID")),
			FriendlyName: nestedString(m, "friendlyName"),
			GuidePath:    nestedString(m, "guidePath"),
			LineupHash:   nestedString(m, "lineupHash"),
			GuideHash:    nestedString(m, "guideHash"),
			Endpoint:     nestedString(m, "endpoint"),
			Port:         int32(port),
		})
	}
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == "Ready" && m["status"] == "True" {
			p.Ready = true
		}
	}
	return p
}

func nestedString(obj map[string]any, path ...string) string {
	v, _, _ := unstructured.NestedString(obj, path...)
	return v
}
