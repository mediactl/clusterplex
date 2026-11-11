package manifests_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every pod running a cluster-plex binary mounts the release's config
// ConfigMap, whose logging: section is the live level knob (the 2026-11-10
// structured-logging design §6): as a directory, read-only, never a subPath
// (a subPath mount never receives the ConfigMap's updates), at
// /etc/clusterplex, or at another directory the container's --log-config
// names a file in (the proxy, whose TLS mount already lives under
// /etc/clusterplex; a mountpoint nested inside the read-only ConfigMap
// directory could not be created).
func TestEveryPodMountsTheConfig(t *testing.T) {
	rendered := renderChart(t)
	var seen int
	for _, d := range docs(t, rendered) {
		kind, name := str(d, "kind"), str(d, "metadata", "name")
		var pod any
		switch kind {
		case "Deployment", "StatefulSet":
			pod = get(d, "spec", "template", "spec")
		case "CronJob":
			pod = get(d, "spec", "jobTemplate", "spec", "template", "spec")
		default:
			continue
		}
		require.NotNil(t, pod, "%s %s has no pod spec", kind, name)
		configVolume := ""
		for _, v := range list(pod, "volumes") {
			if cm := get(v, "configMap", "name"); cm != nil && strings.HasSuffix(str(cm), "-config") {
				configVolume = str(get(v, "name"))
			}
		}
		assert.NotEmpty(t, configVolume, "%s %s has no config ConfigMap volume", kind, name)
		for _, c := range list(pod, "containers") {
			seen++
			var mountPath string
			for _, m := range list(c, "volumeMounts") {
				if str(get(m, "name")) != configVolume {
					continue
				}
				mountPath = str(get(m, "mountPath"))
				assert.Equal(t, true, get(m, "readOnly"), "%s %s/%s mounts the config writable", kind, name, str(get(c, "name")))
				assert.Nil(t, get(m, "subPath"), "%s %s/%s mounts the config with a subPath, which never receives an update", kind, name, str(get(c, "name")))
			}
			if !assert.NotEmpty(t, mountPath, "%s %s/%s does not mount the config ConfigMap", kind, name, str(get(c, "name"))) {
				continue
			}
			if mountPath != "/etc/clusterplex" {
				var named bool
				for _, a := range strs(get(c, "args")) {
					named = named || a == "--log-config="+mountPath+"/config.yaml"
				}
				assert.True(t, named, "%s %s/%s mounts the config at %s and no --log-config names it", kind, name, str(get(c, "name")), mountPath)
			}
			for _, m := range list(c, "volumeMounts") {
				assert.False(t, strings.HasPrefix(str(get(m, "mountPath")), mountPath+"/"),
					"%s %s/%s mounts %s inside the config ConfigMap's directory", kind, name, str(get(c, "name")), str(get(m, "mountPath")))
			}
		}
	}
	assert.Greater(t, seen, 3, "the chart renders fewer containers than it should")
}
