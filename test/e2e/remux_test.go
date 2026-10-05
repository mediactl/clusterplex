//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPlexWebStreamsRunOnTheRemuxPool asks Plex for a DASH stream as Plex
// Web would, and checks the manager routed it to the remux pool and Plex
// served its segments; then asks again and checks the pool answered from its
// cache (docs/superpowers/specs/2026-10-01-browser-remux-transcoder-design.md).
func TestPlexWebStreamsRunOnTheRemuxPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cs := getK8sClient(t)
	pods, _ := waitForCluster(ctx, t, cs)
	pod := pods[0]
	ensureMedia(t, pod)
	section := movieSection(t, pod)
	movieKey, _ := firstMovie(ctx, t, pod, section)

	for i, session := range []string{"remux-a", "remux-b"} {
		before := remuxRoutedOn(t, pod)
		params := "path=%2Flibrary%2Fmetadata%2F" + movieKey + "&mediaIndex=0&partIndex=0&protocol=dash" +
			"&directPlay=0&directStream=1&location=lan&session=" + session +
			"&X-Plex-Product=Plex%20Web&X-Plex-Platform=Chrome&X-Plex-Client-Identifier=e2e-" + session
		st, body := mustPlexCall(t, pod, "GET", "/video/:/transcode/universal/decision?"+params)
		require.Equal(t, 200, st, "transcode decision: %s", body)
		st, body = mustPlexCall(t, pod, "GET", "/video/:/transcode/universal/start.mpd?"+params)
		require.Equal(t, 200, st, "transcode start: %s", body)
		require.Contains(t, body, "chunk-stream", "Plex serves the manifest our transcoder posted")

		require.Eventually(t, func() bool { return remuxRoutedOn(t, pod) > before },
			time.Minute, time.Second, "the manager routed session %s to the remux pool", session)
		seg := fmt.Sprintf("/video/:/transcode/universal/session/%s/0/1.m4s", session)
		require.Eventually(t, func() bool {
			st, _, err := plexCall(pod, "GET", seg)
			return err == nil && st == 200
		}, time.Minute, 2*time.Second, "Plex serves segment 1 of %s from its session directory", session)
		if i == 1 {
			require.Eventually(t, func() bool { return strings.Contains(remuxLogs(t), "remux served from cache") },
				time.Minute, time.Second, "the replay came from the pool's cache")
		}
		stopTranscode(pod, session)
	}
}

// remuxRoutedOn reads clusterplex_jobs_routed_total for jobs the manager
// handed to the remux pool, through the API server's pod proxy.
func remuxRoutedOn(t *testing.T, pod string) float64 {
	out := runCmd(t, "kubectl", "get", "--raw",
		"/api/v1/namespaces/"+namespace+"/pods/"+pod+":probes/proxy/metrics")
	var total float64
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "clusterplex_jobs_routed_total{") && strings.Contains(line, `mode="remux"`) {
			var v float64
			_, _ = fmt.Sscan(strings.Fields(line)[1], &v)
			total += v
		}
	}
	return total
}

// remuxLogs is the remux pool's recent log.
func remuxLogs(t *testing.T) string {
	t.Helper()
	out, _ := exec.Command("kubectl", "-n", namespace, "logs", "-l", "app=plex-remux", "--tail=200").CombinedOutput()
	return string(out)
}
