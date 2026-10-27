package manifests_test

import (
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type placedPod struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec podSpec `yaml:"spec"`
		} `yaml:"template"`
		JobTemplate struct {
			Spec struct {
				Template struct {
					Spec podSpec `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		} `yaml:"jobTemplate"`
	} `yaml:"spec"`
}

type podSpec struct {
	NodeSelector map[string]string `yaml:"nodeSelector"`
	Affinity     map[string]any    `yaml:"affinity"`
	Tolerations  []map[string]any  `yaml:"tolerations"`
}

func (p placedPod) pod() podSpec {
	if p.Kind == "CronJob" {
		return p.Spec.JobTemplate.Spec.Template.Spec
	}
	return p.Spec.Template.Spec
}

// renderPlaced renders the chart with args and returns its maintenance
// CronJobs and its remux, proxy and GPU pool Deployments.
func renderPlaced(t *testing.T, args ...string) []placedPod {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	out, err := exec.Command("helm", append([]string{
		"template", "cp", "../../charts/cluster-plex",
		"--namespace", "media", "--set", "postgres.host=postgres", "--set", "postgres.passwordSecret.name=pg",
		"--set", "proxy.enabled=true", "--set", "remux.enabled=true",
	}, args...)...).CombinedOutput()
	require.NoError(t, err, "%s", out)
	var pods []placedPod
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	for {
		var p placedPod
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else {
			require.NoError(t, err)
		}
		switch {
		case p.Kind == "CronJob",
			p.Kind == "Deployment" && (strings.HasSuffix(p.Metadata.Name, "-remux") || strings.HasSuffix(p.Metadata.Name, "-proxy") ||
				strings.HasSuffix(p.Metadata.Name, "-video-transcode")):
			pods = append(pods, p)
		}
	}
	return pods
}

// TestPlacementKeysReachEveryWorkload: clustarr's control-plane placement
// design (2026-10-21, §5.5) puts cluster-plex's maintenance, remux and proxy
// pods on the workers, so each takes nodeSelector, affinity and tolerations
// the way plex does; the GPU pool takes them too, in place of its Intel
// default.
func TestPlacementKeysReachEveryWorkload(t *testing.T) {
	affinity := `{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"node-role.kubernetes.io/control-plane","operator":"DoesNotExist"}]}]}}}`
	args := []string{"--set", "videoTranscode.enabled=true"}
	for _, block := range []string{"maintenance", "remux", "proxy", "videoTranscode"} {
		args = append(args,
			"--set-json", block+".affinity="+affinity,
			"--set", block+".nodeSelector.kubernetes\\.io/arch=amd64",
			"--set-json", block+`.tolerations=[{"key":"dedicated","operator":"Exists","effect":"NoSchedule"}]`)
	}
	pods := renderPlaced(t, args...)
	require.NotEmpty(t, pods)
	kinds := map[string]bool{}
	for _, p := range pods {
		kinds[p.Kind+"/"+p.Metadata.Name] = true
		spec := p.pod()
		assert.Equal(t, map[string]string{"kubernetes.io/arch": "amd64"}, spec.NodeSelector, p.Metadata.Name)
		assert.Contains(t, spec.Affinity, "nodeAffinity", p.Metadata.Name)
		require.Len(t, spec.Tolerations, 1, p.Metadata.Name)
		assert.Equal(t, "dedicated", spec.Tolerations[0]["key"], p.Metadata.Name)
	}
	var cron, remux, proxy, gpu bool
	for k := range kinds {
		cron = cron || strings.HasPrefix(k, "CronJob/")
		remux = remux || strings.HasSuffix(k, "-remux")
		proxy = proxy || strings.HasSuffix(k, "-proxy")
		gpu = gpu || strings.HasSuffix(k, "-video-transcode")
	}
	assert.True(t, cron && remux && proxy && gpu, "maintenance, remux, proxy and the GPU pool all rendered: %v", kinds)
}

func TestNoPlacementByDefault(t *testing.T) {
	for _, p := range renderPlaced(t) {
		spec := p.pod()
		assert.Empty(t, spec.NodeSelector, p.Metadata.Name)
		assert.Empty(t, spec.Affinity, p.Metadata.Name)
		assert.Empty(t, spec.Tolerations, p.Metadata.Name)
	}
}
