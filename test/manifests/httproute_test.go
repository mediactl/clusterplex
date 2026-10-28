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
	assert.Equal(t, "cp-route", backends[0].(map[string]any)["name"], "the route's own Service")
	assert.EqualValues(t, 32400, backends[0].(map[string]any)["port"])
}

// TestHTTPRouteHasAClusterIPBackend: the route's backend is a ClusterIP twin
// of the client Service -- same pods, same port -- without its
// externalTrafficPolicy Local, which left Cilium's Gateway an endpoint only
// on the Plex pod's node: every other node's Envoy answered "no healthy
// upstream" (talos, 2026-10-28). Nothing renders it with the route off.
func TestHTTPRouteHasAClusterIPBackend(t *testing.T) {
	assert.Nil(t, find(renderObjects(t), "Service", "cp-route"), "a route Service rendered with httpRoute off")

	objs := renderObjects(t, "--set", "plex.replicas=1", "--set", "httpRoute.enabled=true",
		"--set-json", `httpRoute.parentRefs=[{"name":"cilium"}]`)
	svc := find(objs, "Service", "cp-route")
	require.NotNil(t, svc, "no route Service rendered")
	client := find(objs, "Service", "cp")
	require.NotNil(t, client)
	assert.Equal(t, "ClusterIP", svc.str("spec", "type"))
	assert.Nil(t, svc.get("spec", "externalTrafficPolicy"))
	assert.Nil(t, svc.get("spec", "sessionAffinity"))
	assert.Equal(t, client.get("spec", "selector"), svc.get("spec", "selector"), "the client Service's pods")
	ports, _ := svc.get("spec", "ports").([]any)
	require.Len(t, ports, 1)
	assert.EqualValues(t, 32400, ports[0].(map[string]any)["port"])
	assert.Equal(t, "proxy", ports[0].(map[string]any)["targetPort"], "the client Service's target")
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

// TestHTTPRouteTakesExtraRules: httpRoute.extraRules are rendered verbatim
// beside the rule sending every path to Plex -- clustarr's /art to its ui,
// which the Gateway prefers over / as the longer prefix. The provider names
// Plex's public hostname in its image URLs, and without the rule PMS's
// downloads of them hit Plex's 404 (talos, 2026-10-28: 491 movies and 152
// shows lost every image at their next refresh).
func TestHTTPRouteTakesExtraRules(t *testing.T) {
	objs := renderObjects(t, "--set", "plex.replicas=1", "--set", "httpRoute.enabled=true",
		"--set-json", `httpRoute.parentRefs=[{"name":"cilium"}]`,
		"--set-json", `httpRoute.extraRules=[{"matches":[{"path":{"type":"PathPrefix","value":"/art"}}],"backendRefs":[{"name":"clustarr-ui","port":8080}]}]`)
	r := find(objs, "HTTPRoute", "cp")
	require.NotNil(t, r, "no HTTPRoute rendered")
	rules, _ := r.get("spec", "rules").([]any)
	require.Len(t, rules, 2)
	assert.Equal(t, map[string]any{
		"matches":     []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": "/art"}}},
		"backendRefs": []any{map[string]any{"name": "clustarr-ui", "port": 8080}},
	}, rules[0], "the extra rule, verbatim")
	backends, _ := rules[1].(map[string]any)["backendRefs"].([]any)
	require.Len(t, backends, 1)
	assert.Equal(t, "cp-route", backends[0].(map[string]any)["name"], "every other path still reaches Plex")
}
