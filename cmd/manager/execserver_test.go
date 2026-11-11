package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/mediactl/clusterplex/pkg/remoteexec"
	"github.com/mediactl/clusterplex/pkg/video/lease"
	videorelay "github.com/mediactl/clusterplex/pkg/video/relay"
	pb "github.com/mediactl/clusterplex/proto"
)

type nullSink struct{}

func (nullSink) Send(*pb.TranscodeLog) error { return nil }

// Every job a pod runs, whether its own Plex asked through the shim or the
// leader sent it to the worker port, has to start inside Plex's network
// namespace. A helper calls Plex back on 127.0.0.1:32400; started in the pod
// namespace that is the proxy, and Plex sees the call arrive from its link
// subnet instead of loopback.
func TestEveryJobListenerStartsJobsInPlexsNetworkNamespace(t *testing.T) {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "Plex Media Scanner.real"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "Plex Transcoder.real"), []byte("#!/bin/sh\nexit 0\n"), 0o755))

	var mu sync.Mutex
	var started []string
	m := newTestManager()
	m.Config.BinDir = bin
	m.startInPlexNS = func(cmd *exec.Cmd) error {
		mu.Lock()
		started = append(started, filepath.Base(cmd.Path))
		mu.Unlock()
		return cmd.Start()
	}

	shim, worker, err := m.execServices()
	require.NoError(t, err)

	req := func(target string) *pb.ExecRequest {
		return &pb.ExecRequest{TargetBinary: target, Env: map[string]string{"PATH": os.Getenv("PATH")}}
	}
	require.NoError(t, shim.Execute(context.Background(), req("Plex Media Scanner"), nullSink{}))
	// No worker is ready in the fake cluster, so the dispatcher runs the
	// transcode here, and that start has to go through the namespace too.
	require.NoError(t, shim.Execute(context.Background(), req(remoteexec.Transcoder), nullSink{}))
	require.NoError(t, worker.Execute(context.Background(), req(remoteexec.Transcoder), nullSink{}))

	assert.Equal(t, []string{"Plex Media Scanner.real", "Plex Transcoder.real", "Plex Transcoder.real"}, started)
}

// Falling back to a start in the pod namespace would run every helper in a
// network Plex does not expect, and nothing would say so. The manager refuses
// to serve jobs at all rather than serve them from the wrong place.
func TestJobListenersRefuseToStartWithoutPlexsNetworkNamespace(t *testing.T) {
	m := newTestManager()
	m.Config.BinDir = t.TempDir()

	_, _, err := m.execServices()
	require.Error(t, err)
}

// stderrSink keeps what a job told the shim on stderr.
type stderrSink struct {
	mu  sync.Mutex
	err []byte
}

func (s *stderrSink) Send(m *pb.TranscodeLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = append(s.err, m.GetStderrChunk()...)
	return nil
}

// conversionArgs is the captured library HLS job (pkg/video's fixture).
func conversionArgs(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../pkg/video/testdata/plex-video-jobs.jsonl")
	require.NoError(t, err)
	var c struct {
		Args []string `json:"args"`
	}
	require.NoError(t, json.Unmarshal(bytes.SplitN(b, []byte("\n"), 2)[0], &c))
	return c.Args
}

// With a selector the shim's video conversions go to the pool's relay,
// which finds no pod in this fake cluster; without one the dispatcher
// refuses them itself. Either way none runs here.
func TestTheVideoRouteIsWiredWhenASelectorIsSet(t *testing.T) {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "Plex Transcoder.real"), []byte("#!/bin/sh\necho ran-locally >&2\n"), 0o755))
	for selector, says := range map[string]string{
		"app=plex-video-transcode": "video: no-pool: no ready gpu video transcode pod",
		"":                         "video: no-pool: no video transcode pool is configured",
	} {
		m := newTestManager()
		m.Config.BinDir = bin
		m.Config.VideoSelector, m.Config.VideoLeaseName = selector, "cluster-plex-video-transcodes"
		m.startInPlexNS = func(cmd *exec.Cmd) error { return cmd.Start() }
		shim, _, err := m.execServices()
		require.NoError(t, err)
		s := &stderrSink{}
		req := &pb.ExecRequest{TargetBinary: remoteexec.Transcoder, Args: conversionArgs(t), Env: map[string]string{"X_PLEX_TOKEN": "tok"}, Cwd: t.TempDir()}
		require.Error(t, shim.Execute(context.Background(), req, s), selector)
		assert.Contains(t, string(s.err), says, "selector %q", selector)
		assert.NotContains(t, string(s.err), "ran-locally")
	}
}

func TestTheCPUPoolIsWiredWhenItsSelectorIsSet(t *testing.T) {
	m := newTestManager()
	m.Config.VideoSelector, m.Config.VideoLeaseName = "app=plex-video-transcode", "cluster-plex-video-transcodes"
	m.Config.VideoCPUSelector, m.Config.VideoCPUWhen = "app=plex-video-transcode-cpu", "always"
	r := m.videoRelay(m.Config, &videorelay.Placements{})
	require.NotNil(t, r.CPU, "the CPU pool's lister")
	assert.True(t, r.CPUFirst(t.Context()), "always")
	r.Routed(videorelay.PoolCPU)
	r.PassedOver(videorelay.PoolGPU, "full")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.Metrics.VideoConversions.WithLabelValues("cpu")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.Metrics.VideoPassedOver.WithLabelValues("gpu", "full")))

	m.Config.VideoCPUSelector = ""
	r = m.videoRelay(m.Config, &videorelay.Placements{})
	assert.Nil(t, r.CPU, "no selector, no CPU pool")

	// End to end: under always the CPU pool is asked first and the GPU
	// pool last, so with no pod ready in either the refusal names the GPU
	// pool; under never the other way about.
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "Plex Transcoder.real"), []byte("#!/bin/sh\necho ran-locally >&2\n"), 0o755))
	for when, says := range map[string]string{"always": "no ready gpu video transcode pod", "never": "no ready cpu video transcode pod"} {
		m := newTestManager()
		m.Config.BinDir = bin
		m.Config.VideoSelector, m.Config.VideoLeaseName = "app=plex-video-transcode", "cluster-plex-video-transcodes"
		m.Config.VideoCPUSelector, m.Config.VideoCPUWhen = "app=plex-video-transcode-cpu", when
		m.startInPlexNS = func(cmd *exec.Cmd) error { return cmd.Start() }
		shim, _, err := m.execServices()
		require.NoError(t, err)
		s := &stderrSink{}
		req := &pb.ExecRequest{TargetBinary: remoteexec.Transcoder, Args: conversionArgs(t), Env: map[string]string{"X_PLEX_TOKEN": "tok"}, Cwd: t.TempDir()}
		require.Error(t, shim.Execute(context.Background(), req, s), when)
		assert.Contains(t, string(s.err), "video: no-pool: "+says, "when %q", when)
		assert.NotContains(t, string(s.err), "ran-locally")
	}
}

func TestAnUnreadableWorkLeaseIsNotBusy(t *testing.T) {
	// clustarr-busy reads clustarr's Lease: absent, stale or unreadable
	// (RBAC) it is not busy, so the GPU pool is asked first; a failure is
	// logged, never a refusal.
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	cs := fake.NewSimpleClientset()
	cfg := Config{VideoCPUWhen: "clustarr-busy", ClustarrWorkLease: "clustarr-system/clustarr-intel-work"}
	first := cfg.cpuFirst(cs, log)
	assert.False(t, first(t.Context()), "no Lease")

	rt := metav1.NewMicroTime(time.Now())
	_, err := cs.CoordinationV1().Leases("clustarr-system").Create(t.Context(), &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "clustarr-intel-work", Namespace: "clustarr-system", Annotations: map[string]string{lease.WorkAnnotation: "2"}},
		Spec:       coordinationv1.LeaseSpec{RenewTime: &rt},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	assert.True(t, first(t.Context()), "fresh and above zero")

	cs.PrependReactor("get", "leases", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(coordinationv1.Resource("leases"), "clustarr-intel-work", nil)
	})
	assert.False(t, first(t.Context()), "unreadable is not busy")
	assert.Contains(t, logged.String(), "clustarr-intel-work")

	assert.True(t, Config{VideoCPUWhen: "always"}.cpuFirst(cs, log)(t.Context()))
	assert.False(t, Config{VideoCPUWhen: "never"}.cpuFirst(cs, log)(t.Context()))
	assert.False(t, Config{VideoCPUWhen: "clustarr-busy"}.cpuFirst(cs, log)(t.Context()), "no Lease named: never first")
}
