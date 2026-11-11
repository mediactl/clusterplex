package manifests_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
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

// Plex's Logs emptyDir is capped at 1Gi, the hard stop under the tailer's
// deletions (design §4.3).
func TestPlexLogsIsCapped(t *testing.T) {
	for _, d := range docs(t, renderChart(t)) {
		if str(d, "kind") != "StatefulSet" {
			continue
		}
		for _, v := range list(d, "spec", "template", "spec", "volumes") {
			if str(get(v, "name")) == "plex-logs" {
				assert.Equal(t, "1Gi", str(get(v, "emptyDir", "sizeLimit")), "%s: plex-logs has no 1Gi sizeLimit", str(d, "metadata", "name"))
				return
			}
		}
	}
	t.Fatal("no StatefulSet with a plex-logs volume")
}

// loggingSection is the logging: section the chart renders into the config
// ConfigMap's config.yaml under set.
func loggingSection(t *testing.T, set ...string) map[string]any {
	t.Helper()
	for _, d := range docs(t, renderChartWith(t, set...)) {
		if str(d, "kind") != "ConfigMap" || !strings.HasSuffix(str(d, "metadata", "name"), "-config") {
			continue
		}
		var cfg struct {
			Logging map[string]any `yaml:"logging"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(str(d, "data", "config.yaml")), &cfg))
		return cfg.Logging
	}
	t.Fatal("no -config ConfigMap rendered")
	return nil
}

// The logging: section of config.yaml is global.logging with the chart's
// own logging over it key by key, the subsystems merged (the 2026-11-10
// design §5; global.logging is the owner's one knob under clustarr's chart).
func TestTheLoggingSectionRenders(t *testing.T) {
	assert.Equal(t, map[string]any{"level": "info", "format": "json", "addSource": false}, loggingSection(t))
	assert.Equal(t, "debug", loggingSection(t, "global.logging.level=debug")["level"])
	both := loggingSection(t, "global.logging.level=debug", "global.logging.subsystems.livetv=debug",
		"logging.level=error", "logging.subsystems.pms=warn")
	assert.Equal(t, "error", both["level"], "the chart's own level beats the global's")
	assert.Equal(t, map[string]any{"livetv": "debug", "pms": "warn"}, both["subsystems"], "the subsystems merge")
}
