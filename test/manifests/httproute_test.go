package manifests_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHTTPRouteIsOptIn: httpRoute renders one Gateway API HTTPRoute to the
// client Service's port 32400, with the parentRefs and hostnames as given,
// and nothing by default.
func TestHTTPRouteIsOptIn(t *testing.T) {
	assert.Nil(t, find(renderObjects(t), "HTTPRoute", "cp"), "a route rendered with httpRoute off")

	objs := renderObjects(t, "--set", "plex.replicas=1", "--set", "httpRoute.enabled=true",
		"--set-json", `httpRoute.parentRefs=[{"name":"cilium","namespace":"gateway","sectionName":"https"}]`,
		"--set-json", `httpRoute.hostnames=["plex.example.com"]`)
	r := find(objs, "HTTPRoute", "cp")
	require.NotNil(t, r, "no HTTPRoute rendered")
	assert.Equal(t, "gateway.networking.k8s.io/v1", r.str("apiVersion"))
	assert.Equal(t, []any{map[string]any{"name": "cilium", "namespace": "gateway", "sectionName": "https"}}, r.get("spec", "parentRefs"))
	assert.Equal(t, []any{"plex.example.com"}, r.get("spec", "hostnames"))
	rules, _ := r.get("spec", "rules").([]any)
	require.Len(t, rules, 1)
	backends, _ := rules[0].(map[string]any)["backendRefs"].([]any)
	require.Len(t, backends, 1)
	assert.Equal(t, "cp", backends[0].(map[string]any)["name"], "the client Service")
	assert.EqualValues(t, 32400, backends[0].(map[string]any)["port"])
}

// TestHTTPRouteNeedsOnePlexPodAndAParent: through a Gateway every client
// arrives from the Gateway's address, so the client Service's ClientIP
// affinity would pin them all to one pod (ADR-0005); the chart refuses the
// route with more than one Plex pod, and with no parent to attach to.
func TestHTTPRouteNeedsOnePlexPodAndAParent(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"three pods": {[]string{"--set", "plex.replicas=3", "--set-json", `httpRoute.parentRefs=[{"name":"cilium"}]`}, "plex.replicas"},
		"no parent":  {[]string{"--set", "plex.replicas=1"}, "httpRoute.parentRefs"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := exec.Command("helm", append([]string{
				"template", "cp", "../../charts/cluster-plex",
				"--namespace", "media", "--set", "postgres.host=postgres", "--set", "postgres.passwordSecret.name=pg",
				"--set", "httpRoute.enabled=true",
			}, tc.args...)...).CombinedOutput()
			require.Error(t, err, "rendered:\n%s", out)
			assert.True(t, strings.Contains(string(out), tc.want), "%s", out)
		})
	}
	objs := renderObjects(t, "--set", "plex.replicas=1", "--set", "httpRoute.enabled=true", "--set-json", `httpRoute.parentRefs=[{"name":"cilium"}]`)
	require.NotNil(t, find(objs, "HTTPRoute", "cp"))
}
