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

// manifest is any rendered manifest, read as a map.
type manifest map[string]any

func (o manifest) str(path ...string) string {
	v, _ := o.get(path...).(string)
	return v
}

func (o manifest) get(path ...string) any {
	var cur any = map[string]any(o)
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// renderObjects renders the chart with args and returns its objects.
func renderObjects(t *testing.T, args ...string) []manifest {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	out, err := exec.Command("helm", append([]string{
		"template", "cp", "../../charts/cluster-plex",
		"--namespace", "media", "--set", "postgres.host=postgres", "--set", "postgres.passwordSecret.name=pg",
	}, args...)...).CombinedOutput()
	require.NoError(t, err, "%s", out)
	var objs []manifest
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	for {
		// Into a plain map: yaml.v3 gives nested mappings the map type it
		// decodes into, and every lookup here asserts map[string]any.
		var o map[string]any
		if err := dec.Decode(&o); errors.Is(err, io.EOF) {
			return objs
		} else if err != nil {
			require.NoError(t, err)
		}
		if o != nil {
			objs = append(objs, manifest(o))
		}
	}
}

func find(objs []manifest, kind, suffix string) manifest {
	for _, o := range objs {
		if o.str("kind") == kind && strings.HasSuffix(o.str("metadata", "name"), suffix) {
			return o
		}
	}
	return nil
}

// managerConfig is the manager's config.yaml from the chart's ConfigMap.
func managerConfig(t *testing.T, objs []manifest) map[string]any {
	t.Helper()
	cm := find(objs, "ConfigMap", "-config")
	require.NotNil(t, cm, "the manager's ConfigMap")
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(cm.str("data", "config.yaml")), &cfg))
	return cfg
}

func TestTheVideoPoolAsksForOneGPUShareOnAnIntelNode(t *testing.T) {
	objs := renderObjects(t, "--set", "videoTranscode.enabled=true", "--set-json", "videoTranscode.renderGroups=[44,109]")
	d := find(objs, "Deployment", "-video-transcode")
	require.NotNil(t, d, "the GPU pool's Deployment")
	assert.Equal(t, "plex-video-transcode", d.str("spec", "template", "metadata", "labels", "app"))

	pod := d.get("spec", "template", "spec").(map[string]any)
	terms := manifest(pod).get("affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms").([]any)
	expr := terms[0].(map[string]any)["matchExpressions"].([]any)[0].(map[string]any)
	assert.Equal(t, "intel.feature.node.kubernetes.io/gpu", expr["key"])
	assert.Equal(t, []any{"true"}, expr["values"])

	sc := manifest(pod).get("securityContext").(map[string]any)
	assert.Equal(t, true, sc["runAsNonRoot"])
	assert.Equal(t, []any{44, 109}, sc["supplementalGroups"])

	c := manifest(pod["containers"].([]any)[0].(map[string]any))
	assert.Equal(t, "1", c.str("resources", "limits", "gpu.intel.com/i915"))
	csc := c.get("securityContext").(map[string]any)
	assert.Equal(t, true, csc["readOnlyRootFilesystem"])
	assert.Equal(t, false, csc["allowPrivilegeEscalation"])
	args := c.get("args").([]any)
	assert.Contains(t, args, "--max-jobs=3")
	assert.Contains(t, args, "--encoder=h264_qsv")
	mounts := map[string]map[string]any{}
	for _, m := range c.get("volumeMounts").([]any) {
		mounts[m.(map[string]any)["mountPath"].(string)] = m.(map[string]any)
	}
	assert.Equal(t, true, mounts["/media"]["readOnly"], "the library read-only")
	require.Contains(t, mounts, "/run/video-inputs")
	for _, v := range pod["volumes"].([]any) {
		v := manifest(v.(map[string]any))
		if v.str("name") == mounts["/run/video-inputs"]["name"] {
			assert.Equal(t, "Memory", v.str("emptyDir", "medium"), "a streamed subtitle is held in memory, never on disk")
			assert.Equal(t, "64Mi", v.str("emptyDir", "sizeLimit"))
		}
	}
}

func TestADisabledVideoPoolRefusesConversions(t *testing.T) {
	objs := renderObjects(t)
	assert.Nil(t, find(objs, "Deployment", "-video-transcode"))
	cfg := managerConfig(t, objs)
	require.Contains(t, cfg, "video-selector")
	assert.Equal(t, "", cfg["video-selector"], "no pool: the manager refuses every video conversion")
}

func TestTheManagerIsToldTheVideoPool(t *testing.T) {
	cfg := managerConfig(t, renderObjects(t, "--set", "videoTranscode.enabled=true"))
	assert.Equal(t, "app=plex-video-transcode", cfg["video-selector"])
	assert.Equal(t, 50053, cfg["video-port"])
	assert.Equal(t, "cluster-plex-video-transcodes", cfg["video-lease-name"])
}
