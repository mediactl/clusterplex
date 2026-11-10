package collections

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/mediactl/clusterplex/pkg/plexproject"
)

// memberKinds are the clustarr kinds a normal collection holds: each
// one's resource, the clustarr provider that names it in Plex, and its
// Plex type.
var memberKinds = map[string]struct {
	gvr      schema.GroupVersionResource
	provider string
	itemType plexproject.ItemType
}{
	"Movie":  {schema.GroupVersionResource{Group: "catalog.clustarr.io", Version: "v1alpha1", Resource: "movies"}, plexproject.MoviesProvider, plexproject.TypeMovie},
	"Series": {schema.GroupVersionResource{Group: "catalog.clustarr.io", Version: "v1alpha1", Resource: "series"}, plexproject.TVProvider, plexproject.TypeShow},
}

// DynamicMembers resolves members to the GUID cluster-plex's projection
// keys their Plex items by (pkg/plexproject.GUID): plex://<type>/<id>
// when clustarr knows the item's Plex id, else the clustarr provider's
// own, by the object's UID. Each member is read by name.
type DynamicMembers struct {
	Dynamic dynamic.Interface
}

var _ MemberResolver = DynamicMembers{}

// GUIDs reads each member; one clustarr does not have (or of a kind no
// collection holds) is absent from the map. Any other read failure fails
// the whole call, so a sync never acts on a partial answer.
func (d DynamicMembers) GUIDs(ctx context.Context, namespace string, members []Member) (map[Member]string, error) {
	out := make(map[Member]string, len(members))
	for _, m := range members {
		k, ok := memberKinds[m.Kind]
		if !ok {
			continue
		}
		u, err := d.Dynamic.Resource(k.gvr).Namespace(namespace).Get(ctx, m.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s %s/%s: %w", m.Kind, namespace, m.Name, err)
		}
		plexID, _, _ := unstructured.NestedString(u.Object, "status", "metadata", "externalIDs", "plex")
		out[m] = plexproject.GUID(k.provider, k.itemType, string(u.GetUID()), plexID)
	}
	return out, nil
}
