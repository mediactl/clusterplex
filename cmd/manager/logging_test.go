package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/klog/v2"

	"github.com/mediactl/clusterplex/pkg/logging"
)

// firstRecord parses the first JSON line out has.
func firstRecord(t *testing.T, out *bytes.Buffer) map[string]any {
	t.Helper()
	line, _, _ := bytes.Cut(out.Bytes(), []byte("\n"))
	var rec map[string]any
	require.NoError(t, json.Unmarshal(line, &rec), "%s", out.String())
	return rec
}

// The manager's root logger writes the contract's record (the 2026-11-10
// design §3): the process is service manager under namespace cluster-plex,
// the build its version.
func TestTheManagerLogsTheContract(t *testing.T) {
	var out bytes.Buffer
	_, log, err := newRoot(t.Context(), Config{Logging: logging.Options{Format: "json"}}, &out)
	require.NoError(t, err)
	log.Info("started")
	rec := firstRecord(t, &out)
	svc, _ := rec["service"].(map[string]any)
	assert.Equal(t, "manager", svc["name"])
	assert.Equal(t, "cluster-plex", svc["namespace"])
	assert.NotEmpty(t, svc["version"])
	assert.Equal(t, "INFO", rec["level"])
	assert.Equal(t, "started", rec["msg"])
}

// client-go logs through klog; bridged, its lines are records under the
// client-go subsystem with err as the error key.
func TestKlogReachesClientGo(t *testing.T) {
	var out bytes.Buffer
	_, log, err := newRoot(t.Context(), Config{Logging: logging.Options{Format: "json"}}, &out)
	require.NoError(t, err)
	bridgeKlog(log)
	klog.ErrorS(errors.New("boom"), "Failed to watch")
	klog.Flush()
	rec := firstRecord(t, &out)
	assert.Equal(t, "ERROR", rec["level"])
	assert.Equal(t, "client-go", rec["subsystem"])
	assert.Equal(t, "Failed to watch", rec["msg"])
	assert.Equal(t, "boom", rec["err"])
}

// The guide-build Job is its own process: service guide-build, subsystem
// livetv.guide.build.
func TestTheGuideBuildJobNamesItsService(t *testing.T) {
	var out bytes.Buffer
	_, log, err := guideBuildRoot(t.Context(), logging.Options{Format: "json"}, &out)
	require.NoError(t, err)
	log.Info("building")
	rec := firstRecord(t, &out)
	svc, _ := rec["service"].(map[string]any)
	assert.Equal(t, "guide-build", svc["name"])
	assert.Equal(t, "cluster-plex", svc["namespace"])
	assert.Equal(t, "livetv.guide.build", rec["subsystem"])
}
