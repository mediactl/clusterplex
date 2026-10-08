package manifests_test

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The chart replaces k8s/overlays/kind-cluster-plex, and it had drifted from
// k8s/base with nothing to notice: its client Service selected a media proxy
// Deployment the base no longer runs, rather than the Plex pods with the
// base's ClientIP affinity; its Plex container's port was named pms where the
// base's Service targets proxy; and its Role could not delete a Lease. These
// hold the chart's Plex pods, its client and headless Services, its
// disruption budget and its Plex Role to the base, so the two cannot drift
// again.

// chartOnlyEnv is what the chart sets and the base leaves to the manager's
// defaults. Each default names one of the base's objects (cluster-plex-plextv,
// plex-workers, plex-main) or is the value the chart exposes as plex.drainTimeout;
// the chart names its objects after the release, so it has to say which.
var chartOnlyEnv = []string{
	"CLUSTERPLEX_DRAIN_TIMEOUT",
	"CLUSTERPLEX_LEASE_NAME",
	"CLUSTERPLEX_PLEX_EXTERNAL_SERVICE",
	"CLUSTERPLEX_WORKERS_SERVICE",
}

// install is one rendered install, with its Plex StatefulSet and the plex
// container picked out.
type install struct {
	name      string
	namespace string
	all       []doc
	sts       doc
	plex      any
}

func installOf(t *testing.T, name, namespace, rendered string) install {
	t.Helper()
	in := install{name: name, namespace: namespace, all: docs(t, rendered)}
	for _, d := range in.all {
		if str(d, "kind") != "StatefulSet" {
			continue
		}
		require.Nil(t, in.sts, "%s runs Plex in one StatefulSet", name)
		in.sts = d
	}
	require.NotNil(t, in.sts, "%s renders no StatefulSet", name)
	for _, c := range list(in.sts, "spec", "template", "spec", "containers") {
		if str(c, "name") == "plex" {
			in.plex = c
		}
	}
	require.NotNil(t, in.plex, "%s: the StatefulSet has no plex container", name)
	return in
}

func installs(t *testing.T) (base, chart install) {
	t.Helper()
	return installOf(t, "k8s/base", "media", render(t, "k8s/base")),
		installOf(t, "chart", "media", renderChart(t))
}

// asMap reads a decoded mapping: yaml.v3 decodes the maps nested in a doc as
// docs too.
func asMap(v any) map[string]any {
	switch m := v.(type) {
	case doc:
		return m
	case map[string]any:
		return m
	}
	return nil
}

// podLabels are the labels the Plex pods carry.
func (in install) podLabels() map[string]any {
	return asMap(get(in.sts, "spec", "template", "metadata", "labels"))
}

// selectsPlex reports whether selector, a label map, matches the Plex pods.
func (in install) selectsPlex(selector any) bool {
	m := asMap(selector)
	if len(m) == 0 {
		return false
	}
	labels := in.podLabels()
	for k, v := range m {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func (in install) env() map[string]any {
	out := map[string]any{}
	for _, e := range list(in.plex, "env") {
		out[str(e, "name")] = e
	}
	return out
}

func (in install) envValue(name string) string { return str(in.env()[name], "value") }

func (in install) objects(kind string) []doc {
	var out []doc
	for _, d := range in.all {
		if str(d, "kind") == kind {
			out = append(out, d)
		}
	}
	return out
}

func (in install) service(name string) doc {
	for _, s := range in.objects("Service") {
		if str(s, "metadata", "name") == name {
			return s
		}
	}
	return nil
}

// clientService is the one LoadBalancer Service: what clients connect to.
func (in install) clientService(t *testing.T) doc {
	t.Helper()
	var found []doc
	for _, s := range in.objects("Service") {
		if str(s, "spec", "type") == "LoadBalancer" {
			found = append(found, s)
		}
	}
	require.Len(t, found, 1, "%s renders one LoadBalancer Service", in.name)
	return found[0]
}

// inNamespace reports whether an object lands in the install's namespace;
// helm template leaves the namespace off, kustomize writes it.
func (in install) inNamespace(d doc) bool {
	ns := str(d, "metadata", "namespace")
	return ns == "" || ns == in.namespace
}

func portNames(c any) map[string]any {
	out := map[string]any{}
	for _, p := range list(c, "ports") {
		out[str(p, "name")] = get(p, "containerPort")
	}
	return out
}

func mounts(c any) []string {
	var out []string
	for _, m := range list(c, "volumeMounts") {
		out = append(out, fmt.Sprintf("%s at %s readOnly=%v subPath=%q",
			str(m, "name"), str(m, "mountPath"), get(m, "readOnly") == true, str(m, "subPath")))
	}
	sort.Strings(out)
	return out
}

func TestTheChartsPlexPodsAreTheBases(t *testing.T) {
	base, chart := installs(t)

	assert.Equal(t, portNames(base.plex), portNames(chart.plex), "the plex container's ports")
	assert.Equal(t, mounts(base.plex), mounts(chart.plex), "the plex container's volume mounts")

	baseEnv := slices.Sorted(maps.Keys(base.env()))
	chartEnv := slices.Sorted(maps.Keys(chart.env()))
	var onlyBase, onlyChart []string
	for _, n := range baseEnv {
		if !slices.Contains(chartEnv, n) {
			onlyBase = append(onlyBase, n)
		}
	}
	for _, n := range chartEnv {
		if !slices.Contains(baseEnv, n) {
			onlyChart = append(onlyChart, n)
		}
	}
	assert.Empty(t, onlyBase, "the base sets these and the chart does not")
	assert.Equal(t, chartOnlyEnv, onlyChart, "the chart sets these beyond the base")

	for _, field := range []string{"livenessProbe", "readinessProbe", "startupProbe", "resources", "securityContext", "imagePullPolicy"} {
		assert.Equal(t, get(base.plex, field), get(chart.plex, field), "the plex container's %s", field)
	}
	for _, field := range []string{"replicas", "podManagementPolicy"} {
		assert.Equal(t, get(base.sts, "spec", field), get(chart.sts, "spec", field), "the StatefulSet's %s", field)
	}
	assert.Equal(t, get(base.sts, "spec", "template", "spec", "terminationGracePeriodSeconds"),
		get(chart.sts, "spec", "template", "spec", "terminationGracePeriodSeconds"), "terminationGracePeriodSeconds")
	assert.Equal(t, get(base.sts, "spec", "template", "metadata", "annotations"),
		get(chart.sts, "spec", "template", "metadata", "annotations"), "the pods' annotations")

	spread := func(in install) []map[string]any {
		var out []map[string]any
		for _, c := range list(in.sts, "spec", "template", "spec", "topologySpreadConstraints") {
			m := maps.Clone(asMap(c))
			assert.True(t, in.selectsPlex(get(m, "labelSelector", "matchLabels")),
				"%s: a topology spread constraint does not select the Plex pods", in.name)
			delete(m, "labelSelector")
			out = append(out, m)
		}
		return out
	}
	assert.Equal(t, spread(base), spread(chart), "the topology spread constraints")
}

// The client Service is the base's plex-main: every ready Plex pod, pinned by
// client address for three hours, onto the in-pod proxy that holds 32400 in
// front of Plex's network namespace (ADR-0003, ADR-0005's note).
func TestTheChartsClientServiceIsTheBases(t *testing.T) {
	base, chart := installs(t)
	for _, in := range []install{base, chart} {
		svc := in.clientService(t)
		assert.True(t, in.selectsPlex(get(svc, "spec", "selector")),
			"%s: the client Service does not select the Plex pods", in.name)
		for _, p := range list(svc, "spec", "ports") {
			assert.Contains(t, portNames(in.plex), str(p, "targetPort"),
				"%s: the client Service targets a port the plex container does not name", in.name)
		}
	}
	b, c := base.clientService(t), chart.clientService(t)
	for _, field := range []string{"type", "externalTrafficPolicy", "sessionAffinity", "sessionAffinityConfig", "ports"} {
		assert.Equal(t, get(b, "spec", field), get(c, "spec", field), "the client Service's %s", field)
	}

	// The manager advertises this Service's LoadBalancer address; naming
	// another advertises nothing, or the wrong thing.
	assert.Equal(t, str(c, "metadata", "name"), chart.envValue("CLUSTERPLEX_PLEX_EXTERNAL_SERVICE"),
		"CLUSTERPLEX_PLEX_EXTERNAL_SERVICE names the chart's client Service")
}

func TestTheChartsWorkersServiceIsTheBases(t *testing.T) {
	base, chart := installs(t)
	var specs []any
	for _, in := range []install{base, chart} {
		name := str(in.sts, "spec", "serviceName")
		svc := in.service(name)
		require.NotNil(t, svc, "%s: the StatefulSet's serviceName %q is no Service it renders", in.name, name)
		assert.True(t, in.selectsPlex(get(svc, "spec", "selector")),
			"%s: %s does not select the Plex pods", in.name, name)
		spec := maps.Clone(asMap(get(svc, "spec")))
		delete(spec, "selector")
		specs = append(specs, spec)
	}
	assert.Equal(t, specs[0], specs[1], "the headless Service's spec")
	assert.Equal(t, str(chart.sts, "spec", "serviceName"), chart.envValue("CLUSTERPLEX_WORKERS_SERVICE"),
		"CLUSTERPLEX_WORKERS_SERVICE names the chart's headless Service")
}

func TestTheChartsDisruptionBudgetIsTheBases(t *testing.T) {
	base, chart := installs(t)
	var budgets []any
	for _, in := range []install{base, chart} {
		pdbs := in.objects("PodDisruptionBudget")
		require.Len(t, pdbs, 1, in.name)
		assert.True(t, in.selectsPlex(get(pdbs[0], "spec", "selector", "matchLabels")),
			"%s: the PodDisruptionBudget does not select the Plex pods", in.name)
		budgets = append(budgets, get(pdbs[0], "spec", "maxUnavailable"))
	}
	assert.Equal(t, budgets[0], budgets[1], "maxUnavailable")
}

// rules returns the rules of every Role in the install's namespace bound to
// the Plex pods' ServiceAccount, one normalised string per rule.
func (in install) rules(t *testing.T) []string {
	t.Helper()
	sa := str(in.sts, "spec", "template", "spec", "serviceAccountName")
	require.NotEmpty(t, sa, in.name)
	bound := map[string]bool{}
	for _, rb := range in.objects("RoleBinding") {
		if !in.inNamespace(rb) {
			continue
		}
		for _, s := range list(rb, "subjects") {
			if str(s, "kind") == "ServiceAccount" && str(s, "name") == sa {
				bound[str(rb, "roleRef", "name")] = true
			}
		}
	}
	require.NotEmpty(t, bound, "%s: no Role is bound to %s", in.name, sa)
	var out []string
	for _, r := range in.objects("Role") {
		if !in.inNamespace(r) || !bound[str(r, "metadata", "name")] {
			continue
		}
		for _, rule := range list(r, "rules") {
			part := func(key string) string {
				s := strs(get(rule, key))
				sort.Strings(s)
				return key + "=" + strings.Join(s, ",")
			}
			out = append(out, strings.Join([]string{
				part("apiGroups"), part("resources"), part("resourceNames"), part("verbs"),
			}, " "))
		}
	}
	sort.Strings(out)
	return out
}

func TestTheChartsPlexRoleIsTheBases(t *testing.T) {
	base, chart := installs(t)
	assert.Equal(t, base.rules(t), chart.rules(t), "the rules granted to the Plex pods' ServiceAccount")
}
