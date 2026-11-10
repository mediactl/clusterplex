// Package collections reconciles clustarr's PlexCollection resources
// (catalog.clustarr.io/v1alpha1; clustarr's
// docs/superpowers/specs/2026-11-10-plex-collections-design.md) into
// Plex's own collections: a smart one's filter compiled as Plex Web
// encodes it, a normal one's members, and each one's presentation.
package collections

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// Spec mirrors PlexCollection's spec (clustarr's
// api/catalog/v1alpha1/plexcollection_types.go), read from the resource
// through the dynamic client; field names and JSON keys are the same.
type Spec struct {
	Library    string      `json:"library"`
	Title      string      `json:"title"`
	ItemType   string      `json:"itemType"`
	Filter     *Filter     `json:"filter,omitempty"`
	Sort       *Sort       `json:"sort,omitempty"`
	Limit      *int32      `json:"limit,omitempty"`
	Members    *Members    `json:"members,omitempty"`
	Summary    *string     `json:"summary,omitempty"`
	SortTitle  *string     `json:"sortTitle,omitempty"`
	Poster     *Image      `json:"poster,omitempty"`
	Background *Image      `json:"background,omitempty"`
	Settings   *Settings   `json:"settings,omitempty"`
	Visibility *Visibility `json:"visibility,omitempty"`
}

// Filter is a smart collection's filter: the top level's match (all or
// any) and its rules.
type Filter struct {
	Match string `json:"match,omitempty"`
	Rules []Rule `json:"rules"`
}

// Rule is a condition (Field, Operator, Value) or a group (Match, Rules).
type Rule struct {
	Field    string      `json:"field,omitempty"`
	Operator string      `json:"operator,omitempty"`
	Value    string      `json:"value,omitempty"`
	Match    string      `json:"match,omitempty"`
	Rules    []Condition `json:"rules,omitempty"`
}

// Condition is one field compared with a value.
type Condition struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value,omitempty"`
}

// Sort is a smart collection's sort: Plex's sort key and its direction.
type Sort struct {
	Field      string `json:"field"`
	Descending bool   `json:"descending,omitempty"`
}

// Members are a normal collection's items.
type Members struct {
	Items []Member `json:"items"`
}

// Member is a clustarr Movie or Series by name, in the resource's
// namespace.
type Member struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Image is a poster's or background's source URL.
type Image struct {
	URL string `json:"url"`
}

// Settings are the collection's mode and a normal collection's order, in
// words (default, hideCollection, hideItems, showCollectionAndItems;
// releaseDate, alphabetical).
type Settings struct {
	Mode  string `json:"mode,omitempty"`
	Order string `json:"order,omitempty"`
}

// Visibility is where Plex promotes the collection's hub.
type Visibility struct {
	LibraryRecommended bool `json:"libraryRecommended"`
	Home               bool `json:"home"`
	FriendsHome        bool `json:"friendsHome"`
}

// SpecOf reads a PlexCollection's spec.
func SpecOf(u *unstructured.Unstructured) (Spec, error) {
	raw, ok := u.Object["spec"].(map[string]any)
	if !ok {
		return Spec{}, fmt.Errorf("PlexCollection %s/%s: no spec", u.GetNamespace(), u.GetName())
	}
	var s Spec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &s); err != nil {
		return Spec{}, fmt.Errorf("PlexCollection %s/%s: spec: %w", u.GetNamespace(), u.GetName(), err)
	}
	return s, nil
}
