// Package manifests_test holds the kustomize overlays to what they may
// deploy.
package manifests_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func render(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not on PATH")
	}
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
