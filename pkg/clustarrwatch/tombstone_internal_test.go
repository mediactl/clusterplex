package clustarrwatch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
)

// TestATombstoneStillNamesItsFile: an informer that missed a delete hands
// the handler a DeletedFinalStateUnknown, and the folder still needs a scan.
func TestATombstoneStillNamesItsFile(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"path": "/data/media/movies/Heat/Heat.mkv", "mediaRef": map[string]any{"kind": "movie"}},
	}}
	f, ok := fileOf(cache.DeletedFinalStateUnknown{Key: "ns/a", Obj: u})
	assert.True(t, ok)
	assert.Equal(t, "/data/media/movies/Heat/Heat.mkv", f.Path)
}
