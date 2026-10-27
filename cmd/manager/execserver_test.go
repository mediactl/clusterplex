package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/remoteexec"
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
		"app=plex-video-transcode": "video: no-pool: no ready video transcode pod",
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
