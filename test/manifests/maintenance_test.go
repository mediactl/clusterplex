package manifests_test

import (
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	plexroute "github.com/mediactl/clusterplex/pkg/plex/route"
)

// Plex's own scheduler is off on every pod, so the maintenance CronJobs are
// the only thing that runs its upkeep. The kustomize install shipped none:
// on kind-cluster-plex the only CronJobs left were a failed Helm release's,
// calling cp-workers, a Service nothing defined any more, so every run since
// 2026-09-18 failed its DNS lookup six times and was deleted with its logs
// (2026-10-06). These hold both installers to one schedule, and each
// CronJob to a Service its own install defines.

type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Schedule    string `yaml:"schedule"`
		JobTemplate struct {
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Args []string `yaml:"args"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		} `yaml:"jobTemplate"`
	} `yaml:"spec"`
}

func objects(t *testing.T, rendered string) []object {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	var out []object
	for {
		var o object
		if err := dec.Decode(&o); err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			require.NoError(t, err)
		}
		if o.Kind != "" {
			out = append(out, o)
		}
	}
}

// maintenance is one install's CronJobs, by task: its schedule and the
// Service host its trigger calls.
type maintenance map[string]struct{ schedule, host string }

func maintenanceOf(t *testing.T, objs []object) (maintenance, map[string]bool) {
	t.Helper()
	m := maintenance{}
	services := map[string]bool{}
	for _, o := range objs {
		switch o.Kind {
		case "Service":
			services[o.Metadata.Name] = true
		case "CronJob":
			cs := o.Spec.JobTemplate.Spec.Template.Spec.Containers
			require.Len(t, cs, 1, o.Metadata.Name)
			var task, endpoint string
			for _, a := range cs[0].Args {
				if v, ok := strings.CutPrefix(a, "-task="); ok {
					task = v
				}
				if v, ok := strings.CutPrefix(a, "-endpoint="); ok {
					endpoint = v
				}
			}
			require.NotEmpty(t, task, o.Metadata.Name)
			host := strings.TrimPrefix(endpoint, "http://")
			host, _, _ = strings.Cut(host, ".")
			m[task] = struct{ schedule, host string }{o.Spec.Schedule, host}
		}
	}
	return m, services
}

func renderChart(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	out, err := exec.Command("helm", "template", "cp", "../../charts/cluster-plex",
		"--namespace", "media", "--set", "postgres.host=postgres", "--set", "postgres.passwordSecret.name=pg").CombinedOutput()
	require.NoError(t, err, "%s", out)
	return string(out)
}

// unscheduled names the tasks no install may schedule. The analysing ones: clustarr probes
// the files and seeds the streams and markers (ADR 0006), so Plex runs no
// analysis of its own; GenerateBIFBehavior is never, so media-index has
// nothing to do. backup-database copies Plex's SQLite file, and the library
// lives in PostgreSQL.
var unscheduled = []string{"analyze", "deep-analyze", "media-index", "chapter-thumbs", "auto-tags", "backup-database"}

func TestEveryInstallSchedulesTheSameMaintenance(t *testing.T) {
	chart, _ := maintenanceOf(t, objects(t, renderChart(t)))
	for _, overlay := range []string{"k8s/overlays/kind", "k8s/overlays/kind-cluster-plex"} {
		got, _ := maintenanceOf(t, objects(t, render(t, overlay)))
		require.NotEmpty(t, got, "%s schedules no maintenance", overlay)
		require.Len(t, got, len(chart), overlay)
		for task, c := range chart {
			require.Contains(t, got, task, overlay)
			require.Equal(t, c.schedule, got[task].schedule, "%s: %s", overlay, task)
		}
	}
	for _, task := range unscheduled {
		require.NotContains(t, chart, task, "the chart schedules %s", task)
	}
}

func TestEveryMaintenanceCronJobCallsAServiceItsInstallDefines(t *testing.T) {
	for name, rendered := range map[string]string{
		"chart":                          renderChart(t),
		"k8s/overlays/kind":              render(t, "k8s/overlays/kind"),
		"k8s/overlays/kind-cluster-plex": render(t, "k8s/overlays/kind-cluster-plex"),
	} {
		m, services := maintenanceOf(t, objects(t, rendered))
		for task, c := range m {
			require.True(t, services[c.host], "%s: %s calls %q, which it does not define", name, task, c.host)
		}
	}
}

// The maintenance fan-out, like the request router, finds the pods running
// Plex by route.DefaultPlexSelector. The kustomize StatefulSet labelled its
// pods only app: plex, so with the CronJobs calling the right Service every
// task would still have failed "no pod is running Plex".
func TestEveryInstallsPlexPodsMatchTheManagersSelector(t *testing.T) {
	key, value, ok := strings.Cut(plexroute.DefaultPlexSelector, "=")
	require.True(t, ok, "the selector is one key=value: %s", plexroute.DefaultPlexSelector)
	for name, rendered := range map[string]string{
		"chart":                          renderChart(t),
		"k8s/overlays/kind":              render(t, "k8s/overlays/kind"),
		"k8s/overlays/kind-cluster-plex": render(t, "k8s/overlays/kind-cluster-plex"),
	} {
		dec := yaml.NewDecoder(strings.NewReader(rendered))
		sets := 0
		for {
			var o struct {
				Kind string `yaml:"kind"`
				Spec struct {
					ServiceName string `yaml:"serviceName"`
					Template    struct {
						Metadata struct {
							Labels map[string]string `yaml:"labels"`
						} `yaml:"metadata"`
					} `yaml:"template"`
				} `yaml:"spec"`
			}
			if err := dec.Decode(&o); errors.Is(err, io.EOF) {
				break
			} else {
				require.NoError(t, err)
			}
			if o.Kind != "StatefulSet" {
				continue
			}
			sets++
			require.Equal(t, value, o.Spec.Template.Metadata.Labels[key],
				"%s: the Plex pods carry no %s", name, plexroute.DefaultPlexSelector)
		}
		require.Equal(t, 1, sets, "%s runs Plex in one StatefulSet", name)
	}
}
