package main

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"

	plexcollections "github.com/mediactl/clusterplex/pkg/plex/collections"
)

// pmsRestartPoll is how often the collections loop checks whether its
// pod's PMS restarted, which makes it sync every collection again.
const pmsRestartPoll = 30 * time.Second

// collectionsLoop is the lease holder's Plex collections work: clustarr's
// PlexCollections reconciled into this pod's PMS, their status and
// finalizer written by cluster-plex (clustarr's 2026-11-10
// plex-collections design, option A), Events recorded on them.
func (m *Manager) collectionsLoop(ctx context.Context) {
	log := m.Logger.With("component", "collections")
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
		log.Info("collections wait for Plex's identity", "error", err, "retry", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
	ctrl := &plexcollections.Controller{
		Dynamic: m.Dynamic, Discovery: m.K8sClient.Discovery(), Namespace: m.Config.Clustarr.Namespace,
		Syncer: &plexcollections.Syncer{
			PMS: pms, MachineID: machine,
			Members: plexcollections.DynamicMembers{Dynamic: m.Dynamic},
			Fetch:   plexcollections.NewImageFetcher(collectionArtBase(m.Config.Clustarr.UIURL)),
			Now:     time.Now,
		},
		Record: func(obj *unstructured.Unstructured, warning bool, reason, msg string) {
			recordCollectionEvent(recorder, obj, warning, reason, msg)
		},
		Logger: log,
	}
	go m.notePMSRestarts(ctx, ctrl.PMSRestarted)
	if err := ctrl.Run(ctx); err != nil {
		log.Error("reconcile Plex collections", "error", err)
	}
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

// collectionActions names what the lease holder did for each reason, the
// events.k8s.io Event's action.
var collectionActions = map[string]string{
	plexcollections.ActionCreated: "Create",
	plexcollections.ActionAdopted: "Adopt",
	"Deleted":                     "Delete",
}

// recordCollectionEvent records an Event on a PlexCollection through
// client-go's events.k8s.io recorder.
func recordCollectionEvent(recorder events.EventRecorder, obj *unstructured.Unstructured, warning bool, reason, msg string) {
	ref := &corev1.ObjectReference{
		APIVersion: plexcollections.GVR.GroupVersion().String(), Kind: "PlexCollection",
		Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: obj.GetUID(),
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
