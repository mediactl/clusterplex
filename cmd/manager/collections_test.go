package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
)

func TestCollectionArtBase(t *testing.T) {
	for in, want := range map[string]string{
		"https://clustarr.k8s.appkins.io":  "https://clustarr.k8s.appkins.io/art/",
		"https://clustarr.k8s.appkins.io/": "https://clustarr.k8s.appkins.io/art/",
		"":                                 "",
	} {
		assert.Equal(t, want, collectionArtBase(in), in)
	}
}

func TestCollectionEventsLandOnThePlexCollection(t *testing.T) {
	rec := events.NewFakeRecorder(4)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.clustarr.io/v1alpha1", "kind": "PlexCollection",
		"metadata": map[string]any{"name": "up", "namespace": "clustarr-system", "uid": "u1"},
	}}
	recordCollectionEvent(rec, obj, true, "UnknownValue", "the library has no genre \"Nope\"")
	recordCollectionEvent(rec, obj, false, "Adopted", "adopted Plex's collection \"UP\"")
	assert.Equal(t, `Warning UnknownValue the library has no genre "Nope"`, <-rec.Events)
	assert.Equal(t, `Normal Adopted adopted Plex's collection "UP"`, <-rec.Events)
}

func TestCollectionsAccessAsksForEveryVerb(t *testing.T) {
	cs := k8sfake.NewClientset()
	grants := map[string]bool{"list plexcollections": true, "watch plexcollections": true, "patch plexcollections": true}
	var asked []string
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
		r := a.(clienttesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		ra := r.Spec.ResourceAttributes
		key := ra.Verb + " " + ra.Resource
		if ra.Subresource != "" {
			key += "/" + ra.Subresource
		}
		asked = append(asked, key+" in "+ra.Namespace+" ("+ra.Group+")")
		r.Status.Allowed = grants[key]
		return true, r, nil
	})
	allowed := collectionsAccess(cs, "clustarr-system")

	ok, err := allowed(t.Context())
	require.NoError(t, err)
	assert.False(t, ok, "no patch on the status")
	assert.Contains(t, asked, "patch plexcollections/status in clustarr-system (catalog.clustarr.io)")

	grants["patch plexcollections/status"] = true
	ok, err = allowed(t.Context())
	require.NoError(t, err)
	assert.True(t, ok)
}
