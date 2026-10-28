package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	plexlivetv "github.com/mediactl/clusterplex/pkg/plex/livetv"
)

func TestTheLeaseHolderRunsLiveTVRecordsOnTheProviderAndStopsWhenItLetsGo(t *testing.T) {
	// A PMS that takes the device but never lists it: every pass fails that
	// block, which is recorded on the provider.
	var calls atomic.Int32
	pms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/identity" {
			_, _ = io.WriteString(w, `{"MediaContainer":{"machineIdentifier":"machine-a"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"MediaContainer":{}}`)
	}))
	defer pms.Close()

	m := newTestManager()
	m.plexAddr = strings.TrimPrefix(pms.URL, "http://")
	m.Config.Clustarr.Namespace = "clustarr-system"
	m.Config.LiveTV = LiveTVConfig{Enabled: true, Language: "eng", RestartEvery: time.Hour}
	m.Dynamic = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	m.provisionEvery = 20 * time.Millisecond
	m.liveTV = &liveTVFeed{}
	m.liveTV.set([]plexlivetv.Provider{{
		Namespace: "clustarr-system", Name: "polutv", UID: "uid-1", Ready: true, Enabled: true, EPGSource: "XEPG",
		Address: "clustarr-livetv.clustarr-system.svc.cluster.local:80",
		Blocks: []plexlivetv.Block{{
			Start: "1000", DeviceID: "4E624E7A", FriendlyName: "polutv 1000+", GuidePath: "/xmltv.xml",
			Path: "/hdhr/4E624E7A", URI: "http://clustarr-livetv.clustarr-system.svc.cluster.local:80/hdhr/4E624E7A",
		}},
	}})

	m.takePlexTV(t.Context())
	require.Eventually(t, func() bool {
		l, err := m.K8sClient.CoordinationV1().Leases("clustarr-system").Get(t.Context(), "livetv-4e624e7a", metav1.GetOptions{})
		return err == nil && *l.Spec.HolderIdentity == "machine-a"
	}, 5*time.Second, 10*time.Millisecond, "the block's Lease, held as the server's identity")
	require.Eventually(t, func() bool {
		evs, err := m.K8sClient.EventsV1().Events("clustarr-system").List(t.Context(), metav1.ListOptions{})
		if err != nil {
			return false
		}
		for _, e := range evs.Items {
			if e.Regarding.Kind == "IPTVProvider" && e.Regarding.Name == "polutv" && e.Reason == plexlivetv.ReasonFailed && e.Type == "Warning" {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "the failure lands on the provider as an events.k8s.io Event")

	m.releasePlexTV(t.Context())
	time.Sleep(50 * time.Millisecond)
	stopped := calls.Load()
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, stopped, calls.Load(), "no more passes once the lease is gone")
}

func TestAPassThatOnlyWaitsForAGuideLoadPollsSoonAndResetsTheBackoff(t *testing.T) {
	every := 10 * time.Minute
	wait, backoff := nextLiveTVPass(plexlivetv.Waiting, 8*time.Minute, every)
	assert.Equal(t, liveTVGuidePoll, wait, "a guide load ends on its own: no backoff")
	assert.Equal(t, provisionBackoffStart, backoff)

	wait, backoff = nextLiveTVPass(plexlivetv.Pending, 2*time.Minute, every)
	assert.Equal(t, 2*time.Minute, wait, "a failure backs off")
	assert.Equal(t, 4*time.Minute, backoff)

	wait, backoff = nextLiveTVPass(plexlivetv.Converged, 4*time.Minute, every)
	assert.Equal(t, every, wait)
	assert.Equal(t, provisionBackoffStart, backoff)
}
