package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
