package manifests_test

import (
	"errors"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// doc is one rendered object, read loosely.
type doc map[string]any

func docs(t *testing.T, rendered string) []doc {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	var out []doc
	for {
		var d doc
		if err := dec.Decode(&d); err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			require.NoError(t, err)
		}
		if d != nil {
			out = append(out, d)
		}
	}
}

func get(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(doc)
		if !ok {
			mm, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			m = mm
		}
		v = m[p]
	}
	return v
}

func str(v any, path ...string) string {
	s, _ := get(v, path...).(string)
	return s
}

func list(v any, path ...string) []any {
	l, _ := get(v, path...).([]any)
	return l
}

func strs(v any) []string {
	var out []string
	for _, s := range list(v) {
		out = append(out, s.(string))
	}
	return out
}

// tokenSecretGrant checks one install: the manager's config names a token
// Secret in clustarr's namespace, the Plex pods' ServiceAccount may read and
// write exactly that Secret there and create it, and the pods take the claim
// code from an optional Secret.
func tokenSecretGrant(t *testing.T, rendered string) {
	t.Helper()
	all := docs(t, rendered)

	var namespace, secret string
	for _, d := range all {
		if str(d, "kind") != "ConfigMap" {
			continue
		}
		raw := str(d, "data", "config.yaml")
		if raw == "" {
			continue
		}
		var cfg map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(raw), &cfg))
		if s := str(cfg, "plex", "clustarr", "tokenSecret"); s != "" {
			namespace, secret = str(cfg, "plex", "clustarr", "namespace"), s
		}
	}
	require.NotEmpty(t, secret, "the manager's config names no token Secret")

	var sa string
	for _, d := range all {
		if str(d, "kind") == "StatefulSet" {
			sa = str(d, "spec", "template", "spec", "serviceAccountName")
			var claim any
			for _, e := range list(d, "spec", "template", "spec", "containers") {
				for _, env := range list(e, "env") {
					if str(env, "name") == "CLUSTERPLEX_PLEX_CLAIM" {
						claim = env
					}
				}
			}
			require.NotNil(t, claim, "the Plex pods take no claim code")
			assert.Equal(t, true, get(claim, "valueFrom", "secretKeyRef", "optional"),
				"an absent claim Secret must not keep Plex from starting")
		}
	}
	require.NotEmpty(t, sa)

	roles := map[string]bool{}
	for _, d := range all {
		if str(d, "kind") != "RoleBinding" || str(d, "metadata", "namespace") != namespace {
			continue
		}
		for _, s := range list(d, "subjects") {
			if str(s, "kind") == "ServiceAccount" && str(s, "name") == sa {
				roles[str(d, "roleRef", "name")] = true
			}
		}
	}
	var readWrite, create bool
	for _, d := range all {
		if str(d, "kind") != "Role" || str(d, "metadata", "namespace") != namespace || !roles[str(d, "metadata", "name")] {
			continue
		}
		for _, r := range list(d, "rules") {
			if !slices.Contains(strs(get(r, "resources")), "secrets") {
				continue
			}
			verbs, names := strs(get(r, "verbs")), strs(get(r, "resourceNames"))
			assert.NotContains(t, verbs, "list", "the manager never lists Secrets")
			assert.NotContains(t, verbs, "watch", "the manager never watches Secrets")
			if slices.Contains(verbs, "get") && slices.Contains(verbs, "update") {
				assert.Equal(t, []string{secret}, names, "read and write only the token Secret")
				readWrite = true
			}
			if slices.Contains(verbs, "create") {
				create = true
			}
		}
	}
	assert.True(t, readWrite, "%s may not get and update %s/%s", sa, namespace, secret)
	assert.True(t, create, "%s may not create %s/%s", sa, namespace, secret)
}

func TestTheChartLetsTheManagerKeepTheTokenSecret(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	trackInputs(t)
	out, err := exec.Command("helm", "template", "cp", "../../charts/cluster-plex", "--namespace", "media",
		"--set", "postgres.host=postgres", "--set", "postgres.passwordSecret.name=pg",
		"--set", "plex.clustarr.enabled=true", "--set", "plex.clustarr.tokenSecret=plex-token",
		"--set", "plex.clustarr.pathMappings[0].clustarr=/data/media", "--set", "plex.clustarr.pathMappings[0].plex=/library",
	).CombinedOutput()
	require.NoError(t, err, "%s", out)
	tokenSecretGrant(t, string(out))
}

func TestTheLiveOverlayLetsTheManagerKeepTheTokenSecret(t *testing.T) {
	tokenSecretGrant(t, render(t, "k8s/overlays/kind-cluster-plex"))
}
