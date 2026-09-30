package main

import (
	"context"
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
