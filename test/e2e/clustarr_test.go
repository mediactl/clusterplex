//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestClustarrLibraryIsProvisionedWithoutTheUI: the library comes up on the
// clustarr agent from config.yaml alone, and a new MediaFile makes the lease
// holder scan its folder.
func TestClustarrLibraryIsProvisionedWithoutTheUI(t *testing.T) {
	// The stand-ins come only from `make deploy-kind-clustarr`, whose guard
	// refuses a cluster running a real clustarr; without them there is
	// nothing to provision against.
	if out, _ := exec.Command("kubectl", "get", "crd", "mediafiles.catalog.clustarr.io",
		"-l", "clusterplex.mediactl.io/e2e-fixture=true", "-o", "name").Output(); len(strings.TrimSpace(string(out))) == 0 {
		t.Skip("the clustarr fixture is not deployed: run make deploy-kind-clustarr on a throwaway kind cluster")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cs := getK8sClient(t)
	pods, _ := waitForCluster(ctx, t, cs)
	pod := pods[0]
	execPod(t, pod, "mkdir", "-p", "/media/fixture-movies/Heat (1995)")

	waitFor(ctx, t, "the clustarr library on the clustarr agent", func() (bool, error) {
		_, body := mustPlexCall(t, pod, "GET", "/library/sections")
		return strings.Contains(body, `title="Clustarr Fixture Movies"`) &&
			strings.Contains(body, `agent="tv.plex.agents.custom.clustarr.movies"`), nil
	})

	leader := strings.TrimSpace(runCmd(t, "kubectl", "-n", namespace, "get", "lease", leaseName,
		"-o", "jsonpath={.spec.holderIdentity}"))
	before := scansOn(t, leader)
	manifest := filepath.Join(t.TempDir(), "mediafile.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(`apiVersion: catalog.clustarr.io/v1alpha1
kind: MediaFile
metadata: {name: heat, namespace: clustarr-system}
spec: {path: "/data/media/fixture-movies/Heat (1995)/Heat (1995).mkv", mediaRef: {kind: movie, name: heat}}
`), 0o644))
	runCmd(t, "kubectl", "apply", "-f", manifest)
	waitFor(ctx, t, "a folder scan from the lease holder", func() (bool, error) {
		return scansOn(t, leader) > before, nil
	})
}

// scansOn reads clusterplex_clustarr_scans_total{scope="folder"} from a
// pod's metrics through the API server's pod proxy.
func scansOn(t *testing.T, pod string) float64 {
	out := runCmd(t, "kubectl", "get", "--raw",
		"/api/v1/namespaces/"+namespace+"/pods/"+pod+":probes/proxy/metrics")
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, `clusterplex_clustarr_scans_total{scope="folder"}`) {
			var v float64
			_, _ = fmt.Sscan(strings.Fields(line)[1], &v)
			return v
		}
	}
	return 0
}
