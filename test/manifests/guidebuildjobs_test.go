package manifests_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renderChartWith renders the chart as renderChart does, with more values.
func renderChartWith(t *testing.T, set ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	trackInputs(t)
	args := []string{
		"template", "cp", "../../charts/cluster-plex", "--namespace", "media",
		"--set", "postgres.host=postgres", "--set", "postgres.passwordSecret.name=pg",
	}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	require.NoError(t, err, "%s", out)
	return string(out)
}

// jobRules are the rules on batch Jobs among rules.
func jobRules(rules []string) []string {
	var out []string
	for _, r := range rules {
		if strings.Contains(r, "apiGroups=batch ") && strings.Contains(r, "resources=jobs ") {
			out = append(out, r)
		}
	}
	return out
}

// TestTheGuideBuildJobsComponentIsTheChartsJobRule: the managers build a
// Live TV guide in a Job (plex.liveTV.cloudGuide or xmltvGuide "manager",
// cloudGuideBuild "job"), which the chart grants its Plex pods'
// ServiceAccount and the base does not, so a kustomize install that builds
// guides takes k8s/components/guide-build-jobs (the kind overlay does, the
// guide rig being one): its rule is the chart's, verb for verb. Without it
// the rig's first build Job failed "cannot get resource jobs" (2026-11-11).
func TestTheGuideBuildJobsComponentIsTheChartsJobRule(t *testing.T) {
	built := map[string][]string{
		"an XMLTV guide built in a Job": {"plex.replicas=1", "plex.liveTV.enabled=true", "plex.liveTV.xmltvGuide=manager"},
		"a cloud guide built in a Job":  {"plex.replicas=1", "plex.liveTV.enabled=true", "plex.liveTV.cloudGuide=manager"},
	}
	kind := installOf(t, "k8s/overlays/kind", "media", render(t, "k8s/overlays/kind"))
	got := jobRules(kind.rules(t))
	for name, set := range built {
		chart := installOf(t, "chart ("+name+")", "media", renderChartWith(t, set...))
		want := jobRules(chart.rules(t))
		require.NotEmpty(t, want, "the chart grants no Jobs for %s", name)
		assert.Equal(t, want, got, "the kind overlay's Job rule against the chart's, %s", name)
	}

	// Neither grants a Job where no guide is built in one.
	base, chart := installs(t)
	assert.Empty(t, jobRules(base.rules(t)), "k8s/base")
	assert.Empty(t, jobRules(chart.rules(t)), "the chart by default")
	inProcess := installOf(t, "chart (built in process)", "media", renderChartWith(t,
		"plex.replicas=1", "plex.liveTV.enabled=true", "plex.liveTV.xmltvGuide=manager", "plex.liveTV.cloudGuideBuild=inProcess"))
	assert.Empty(t, jobRules(inProcess.rules(t)), "the chart building in process")
}
