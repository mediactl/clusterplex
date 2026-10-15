package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
	"github.com/mediactl/clusterplex/pkg/plexproject"
)

// fakeReconciler stands in for the watcher on a holder.
type fakeReconciler struct{ keys, folders []string }

func (f *fakeReconciler) ReconcileSection(_ context.Context, key, folder string) (plexproject.Outcome, bool, error) {
	f.keys = append(f.keys, key)
	f.folders = append(f.folders, folder)
	return plexproject.Outcome{Applied: 3}, key == "1", nil
}

func scanRequest(target string, forwarded bool) *http.Request {
	r := httptest.NewRequest(http.MethodPost, target, nil)
	if forwarded {
		r.Header.Set(forwardedHeader, "1")
	}
	return r
}

func withSections(m *Manager) {
	m.recordSections(map[string]string{"Clustarr Movies": "1"}, func(string) (int64, error) { return 10, nil })
}

// The holder runs the reconcile and answers its outcome.
func TestTheScanEndpointReconcilesOnTheHolder(t *testing.T) {
	m := newTestManager()
	withSections(m)
	rec := &fakeReconciler{}
	m.reconciler = rec
	m.stopLeader = func() {} // holding
	rr := httptest.NewRecorder()
	m.probeHandler().ServeHTTP(rr, scanRequest("/api/v1/scan?section=1&path=%2Fmedia%2Fmovies%2FHeat", false))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out plexproject.Outcome
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Equal(t, 3, out.Applied)
	require.Equal(t, []string{"1"}, rec.keys)
	require.Equal(t, []string{"/media/movies/Heat"}, rec.folders)
}

// A section the projection does not own is 404, so the proxy's divert
// lets the request go on to Plex.
func TestTheScanEndpointSays404ForAnUnprovisionedSection(t *testing.T) {
	m := newTestManager()
	withSections(m)
	m.reconciler = &fakeReconciler{}
	m.stopLeader = func() {}
	rr := httptest.NewRecorder()
	m.probeHandler().ServeHTTP(rr, scanRequest("/api/v1/scan?section=9", false))
	require.Equal(t, http.StatusNotFound, rr.Code)
}

// A non-holder forwards to the pod the Lease names, by its pod IP, and
// relays the answer; a forwarded request that still lands on a non-holder
// (the lease moved) is 409, never bounced again.
func TestANonHolderForwardsTheScanToTheLeaseHolder(t *testing.T) {
	m := newTestManager()
	withSections(m)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "1", r.Header.Get(forwardedHeader))
		require.Equal(t, "/api/v1/scan", r.URL.Path)
		require.Equal(t, "1", r.URL.Query().Get("section"))
		_, _ = w.Write([]byte(`{"Applied":7}`))
	}))
	t.Cleanup(holder.Close)
	host, port, _ := strings.Cut(strings.TrimPrefix(holder.URL, "http://"), ":")
	m.Config.ProbePort = atoiT(t, port)
	m.Config.LeaseName = "cluster-plex-plextv"
	identity := "plex-1"
	_, err := m.K8sClient.CoordinationV1().Leases(m.Config.Namespace).Create(t.Context(), &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: m.Config.LeaseName, Namespace: m.Config.Namespace},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &identity, LeaseDurationSeconds: new(int32(3600)), RenewTime: &metav1.MicroTime{Time: metav1.Now().Time}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = m.K8sClient.CoreV1().Pods(m.Config.Namespace).Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: identity, Namespace: m.Config.Namespace},
		Status:     corev1.PodStatus{PodIP: host},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	m.probeHandler().ServeHTTP(rr, scanRequest("/api/v1/scan?section=1", false))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.JSONEq(t, `{"Applied":7}`, rr.Body.String())

	rr = httptest.NewRecorder()
	m.probeHandler().ServeHTTP(rr, scanRequest("/api/v1/scan?section=1", true))
	require.Equal(t, http.StatusConflict, rr.Code)
}

// No pod holds the lease: 503, so the client sees why, and the proxy
// never falls back to Plex's own scan.
func TestAScanWithNoHolderFailsFast(t *testing.T) {
	m := newTestManager()
	withSections(m)
	m.Config.LeaseName = "cluster-plex-plextv"
	rr := httptest.NewRecorder()
	m.probeHandler().ServeHTTP(rr, scanRequest("/api/v1/scan?section=1", false))
	require.Equal(t, http.StatusServiceUnavailable, rr.Code)
	require.Contains(t, rr.Body.String(), "no lease holder")
}

// provisionedKeys is the record on a holder, and the provisioned library
// names resolved through Plex's section list elsewhere.
func TestProvisionedKeysFallBackToPlexsSectionsByName(t *testing.T) {
	m := newTestManager()
	withSections(m)
	require.Equal(t, map[string]bool{"1": true}, m.provisionedKeys(t.Context()))
	m.recordSections(map[string]string{}, nil)
	m.Config.Provision.Libraries = nil
	require.Empty(t, m.provisionedKeys(t.Context()), "no record, no libraries: nothing")
}

func atoiT(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		require.True(t, c >= '0' && c <= '9')
		n = n*10 + int(c-'0')
	}
	return n
}

var _ clustarrwatch.SectionIDs
