package livetv

import (
	"context"
	"log/slog"
	"strings"
	"sync"
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
	// OnChange is told the providers now, and those deleted since the last
	// call, after Debounce of quiet.
	OnChange func(live, gone []Provider)
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
	var (
		mu   sync.Mutex
		gone = map[string]Provider{}
	)
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
		DeleteFunc: func(obj any) {
			if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = t.Obj
			}
			if u, ok := obj.(*unstructured.Unstructured); ok {
				p := ProviderOf(u)
				mu.Lock()
				gone[p.key()] = p
				mu.Unlock()
			}
			wake()
		},
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
		mu.Lock()
		var g []Provider
		for k, p := range gone {
			g = append(g, p)
			delete(gone, k)
		}
		mu.Unlock()
		w.OnChange(live, g)
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

// ProviderOf reads the fields the converger needs from an IPTVProvider.
func ProviderOf(u *unstructured.Unstructured) Provider {
	str := func(path ...string) string {
		v, _, _ := unstructured.NestedString(u.Object, path...)
		return v
	}
	p := Provider{
		Namespace: u.GetNamespace(), Name: u.GetName(), UID: string(u.GetUID()),
		DeviceID:     strings.ToUpper(str("status", "deviceID")),
		Address:      str("status", "address"),
		GuideURL:     str("status", "guideURL"),
		FriendlyName: str("spec", "device", "friendlyName"),
		EPGSource:    str("spec", "epgSource"),
		LineupHash:   str("status", "lineup", "hash"),
		GuideHash:    str("status", "guideHash"),
		Enabled:      true,
	}
	if v, found, _ := unstructured.NestedBool(u.Object, "spec", "enabled"); found {
		p.Enabled = v
	}
	if p.EPGSource == "" {
		p.EPGSource = "XEPG"
	}
	if p.FriendlyName == "" {
		p.FriendlyName = "Clustarr " + p.Name
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
