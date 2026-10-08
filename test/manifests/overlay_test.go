// Package manifests_test holds the kustomize overlays to what they may
// deploy.
package manifests_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// trackInputs opens every file of the manifests and the chart, so that go
// test's cache sees them. kubectl and helm read them in a subprocess, which
// the cache cannot see: without this an edited template could pass on the
// result of the template before it.
func trackInputs(t *testing.T) {
	t.Helper()
	for _, root := range []string{"../../k8s", "../../charts/cluster-plex"} {
		require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			return f.Close()
		}))
	}
}

func render(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not on PATH")
	}
	trackInputs(t)
	out, err := exec.Command("kubectl", "kustomize", "../../"+dir).CombinedOutput()
	require.NoError(t, err, "%s", out)
	return string(out)
}

// TestTheKindOverlayCarriesNothingOfClustarrs: `make deploy-kind` applies
// this overlay to whatever context is current, and kind-cluster-plex also
// runs the owner's clustarr. A stand-in CRD applied there would replace
// clustarr's own (dropping its status subresource), and deleting the overlay
// would delete every Movie, Series, Episode and MediaFile with it.
func TestTheKindOverlayCarriesNothingOfClustarrs(t *testing.T) {
	out := render(t, "k8s/overlays/kind")
	require.NotContains(t, out, "kind: CustomResourceDefinition")
	require.NotContains(t, out, "clustarr-system")
}

// TestTheClustarrFixtureIsLabelledSoTheGuardCanTellItApart: the fixture
// overlay's guard refuses a cluster holding a catalog.clustarr.io CRD
// without this label, which is how it tells a real clustarr from a
// previous fixture run.
func TestTheClustarrFixtureIsLabelledSoTheGuardCanTellItApart(t *testing.T) {
	out := render(t, "k8s/overlays/kind-clustarr")
	docs := strings.Split(out, "\n---\n")
	crds := 0
	for _, d := range docs {
		if strings.Contains(d, "kind: CustomResourceDefinition") {
			crds++
			require.Contains(t, d, "clusterplex.mediactl.io/e2e-fixture: \"true\"", d)
		}
	}
	require.Equal(t, 4, crds)
}
