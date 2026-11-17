package main

import (
	"context"
	"strings"
	"time"

	"github.com/mediactl/clusterplex/pkg/logging"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"

	plexcollections "github.com/mediactl/clusterplex/pkg/plex/collections"
	plexprovision "github.com/mediactl/clusterplex/pkg/plex/provision"
)

// pmsRestartPoll is how often the collections loop checks whether its
// pod's PMS restarted, which makes it sync every collection again.
const pmsRestartPoll = 30 * time.Second

// collectionsLoop is the lease holder's Plex collections work: clustarr's
// PlexCollections reconciled into this pod's PMS, their status and
// finalizer written by cluster-plex (clustarr's 2026-11-10
// plex-collections design, option A), Events recorded on them; and its
// PlexHubLayouts reconciled into each library's managed hubs.
func (m *Manager) collectionsLoop(ctx context.Context) {
	log := logging.SubsystemLogger(m.Logger, "collections")
	broadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: m.K8sClient.EventsV1()})
	broadcaster.StartRecordingToSink(ctx.Done())
	defer broadcaster.Shutdown()
	recorder := broadcaster.NewRecorder(scheme.Scheme, "cluster-plex")

	pms := m.pms()
	var machine string
	for backoff := 5 * time.Second; ; backoff = min(2*backoff, 2*time.Minute) {
		id, err := pms.Identity(ctx)
		if err == nil {
			machine = id
			break
		}
		log.Info("collections wait for Plex's identity", "err", err, "retry", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
	// The hubs of each library -- their order, and where each is promoted
	// -- are clustarr's PlexHubLayouts (the 2026-11-16 plex-hub-layout
	// design): the status is this pod's to write, the spec never.
	sections := plexcollections.SectionFinder(m.Config.Provision.SectionsTitled)
	hubs := &plexcollections.HubController{
		Dynamic: m.Dynamic, Discovery: m.K8sClient.Discovery(), Namespace: m.Config.Clustarr.Namespace,
		Syncer: &plexcollections.HubSyncer{PMS: pms, Sections: sections},
		Record: func(obj *unstructured.Unstructured, warning bool, reason, msg string) {
			recordCollectionEvent(recorder, obj, warning, reason, msg)
		},
		Allowed: hubLayoutsAccess(m.K8sClient, m.Config.Clustarr.Namespace),
		Ready:   m.plexAnswers,
		Logger:  logging.SubsystemLogger(m.Logger, "collections.hubs"),
	}
	ctrl := &plexcollections.Controller{
		Dynamic: m.Dynamic, Discovery: m.K8sClient.Discovery(), Namespace: m.Config.Clustarr.Namespace,
		Syncer: &plexcollections.Syncer{
			PMS: pms, MachineID: machine,
			Members: plexcollections.DynamicMembers{Dynamic: m.Dynamic},
			Catalog: plexcollections.ProviderCatalog{Providers: collectionProviders(m.Config.Provision)},
			Fetch:   plexcollections.NewImageFetcher(collectionArtBase(m.Config.Clustarr.UIURL)),
			Now:     time.Now,
			// A library is found by a title it had, too (plex.libraries'
			// formerNames): a collection's spec.library never changes.
			Sections: sections,
		},
		Record: func(obj *unstructured.Unstructured, warning bool, reason, msg string) {
			recordCollectionEvent(recorder, obj, warning, reason, msg)
		},
		Allowed: collectionsAccess(m.K8sClient, m.Config.Clustarr.Namespace),
		// A collection a layout lists takes its hub's visibility from it.
		HubLayouts: hubs.Lists,
		Logger:     log,
	}
	go m.notePMSRestarts(ctx, func() {
		ctrl.PMSRestarted()
		hubs.PMSRestarted()
	})
	go func() {
		if err := hubs.Run(ctx); err != nil {
			hubs.Logger.Error("reconcile Plex hub layouts", "err", err)
		}
	}()
	if err := ctrl.Run(ctx); err != nil {
		log.Error("reconcile Plex collections", "err", err)
	}
}

// collectionsAccess asks the apiserver whether this pod may do what the
// collections controller does in namespace: list, watch and patch (the
// finalizer) PlexCollections, and patch their status. A chart older than
// the image grants none of it, and the controller waits for it.
func collectionsAccess(cs kubernetes.Interface, namespace string) func(context.Context) (bool, error) {
	return resourceAccess(cs, namespace, []authorizationv1.ResourceAttributes{
		{Verb: "list", Resource: "plexcollections"},
		{Verb: "watch", Resource: "plexcollections"},
		{Verb: "patch", Resource: "plexcollections"},
		{Verb: "patch", Resource: "plexcollections", Subresource: "status"},
	})
}

// hubLayoutsAccess asks whether this pod may do what the hub layout
// controller does in namespace: get, list and watch PlexHubLayouts, patch
// their status, and list and watch the PlexCollections they name.
func hubLayoutsAccess(cs kubernetes.Interface, namespace string) func(context.Context) (bool, error) {
	return resourceAccess(cs, namespace, []authorizationv1.ResourceAttributes{
		{Verb: "get", Resource: "plexhublayouts"},
		{Verb: "list", Resource: "plexhublayouts"},
		{Verb: "watch", Resource: "plexhublayouts"},
		{Verb: "patch", Resource: "plexhublayouts", Subresource: "status"},
		{Verb: "list", Resource: "plexcollections"},
		{Verb: "watch", Resource: "plexcollections"},
	})
}

// resourceAccess asks the apiserver whether this pod is allowed every one
// of checks on clustarr's catalog group in namespace.
func resourceAccess(cs kubernetes.Interface, namespace string, checks []authorizationv1.ResourceAttributes) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		for _, ra := range checks {
			ra.Group = plexcollections.GVR.Group
			ra.Namespace = namespace
			r, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
				Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &ra},
			}, metav1.CreateOptions{})
			if err != nil {
				return false, err
			}
			if !r.Status.Allowed {
				return false, nil
			}
		}
		return true, nil
	}
}

// plexAnswers is whether Plex may be asked for hub work now: a pod that
// runs Plex waits until it serves.
func (m *Manager) plexAnswers() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.runsPlex || m.plexServing
}

// notePMSRestarts calls restarted whenever this pod's PMS starts again.
func (m *Manager) notePMSRestarts(ctx context.Context, restarted func()) {
	started := func() time.Time {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.pmsStarted
	}
	last := started()
	t := time.NewTicker(pmsRestartPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if now := started(); !now.Equal(last) {
			last = now
			restarted()
		}
	}
}

// collectionArtBase is clustarr UI's /art/ at its external URL, or "".
func collectionArtBase(uiURL string) string {
	if uiURL == "" {
		return ""
	}
	return strings.TrimRight(uiURL, "/") + "/art/"
}

// collectionProviders are the metadata providers a TMDB collection is
// asked of: clustarr's, as plex.metadataProviders lists them. The one
// that holds movies answers; the TV one says 404.
func collectionProviders(prov plexprovision.Config) []string {
	uris := make([]string, 0, len(prov.Providers))
	for _, p := range prov.Providers {
		if p.URI != "" {
			uris = append(uris, p.URI)
		}
	}
	return uris
}

// collectionActions names what the lease holder did for each reason, the
// events.k8s.io Event's action.
var collectionActions = map[string]string{
	plexcollections.ActionCreated: "Create",
	plexcollections.ActionAdopted: "Adopt",
	"Deleted":                     "Delete",
	"Applied":                     "Apply",
}

// recordCollectionEvent records an Event on a PlexCollection, or on the
// PlexHubLayout obj is, through client-go's events.k8s.io recorder.
func recordCollectionEvent(recorder events.EventRecorder, obj *unstructured.Unstructured, warning bool, reason, msg string) {
	ref := &corev1.ObjectReference{
		APIVersion: plexcollections.GVR.GroupVersion().String(), Kind: "PlexCollection",
		Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: obj.GetUID(),
	}
	if k := obj.GetKind(); k != "" {
		ref.Kind = k
	}
	kind := corev1.EventTypeNormal
	if warning {
		kind = corev1.EventTypeWarning
	}
	action := collectionActions[reason]
	if action == "" {
		action = "Sync"
	}
	recorder.Eventf(ref, nil, kind, reason, action, "%s", msg)
}
