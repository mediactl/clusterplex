package worker_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/worker"
)

func TestSlotsHoldOneBackForStreams(t *testing.T) {
	s := worker.NewSlots(3)
	_, ok := s.Acquire("A", video.OutputHLS, func() {})
	require.True(t, ok, "stream A")
	_, ok = s.Acquire("F", video.OutputFile, func() {})
	require.True(t, ok, "a download with two slots free")
	_, ok = s.Acquire("G", video.OutputFile, func() {})
	require.False(t, ok, "a second download would take the last slot a stream could need")
	_, ok = s.Acquire("B", video.OutputHLS, func() {})
	require.True(t, ok, "stream B takes the last slot")
	_, ok = s.Acquire("C", video.OutputHLS, func() {})
	require.False(t, ok, "a fourth job on a full pod")
}

func TestARestartTakesItsSessionsSlot(t *testing.T) {
	s := worker.NewSlots(3)
	cancelled := false
	releaseA, _ := s.Acquire("A", video.OutputHLS, func() { cancelled = true })
	s.Acquire("B", video.OutputHLS, func() {})
	s.Acquire("C", video.OutputHLS, func() {})
	require.Zero(t, s.Free())

	releaseA2, ok := s.Acquire("A", video.OutputHLS, func() {})
	require.True(t, ok, "Plex restarted session A (a seek) before its old job let go")
	require.True(t, cancelled, "the old job of session A is cancelled")
	require.Zero(t, s.Free())

	releaseA() // the old job ending must not free the new one's slot
	require.Zero(t, s.Free())
	releaseA2()
	require.Equal(t, 1, s.Free())
}

func TestFreeCountsWhatIsLeft(t *testing.T) {
	s := worker.NewSlots(2)
	require.Equal(t, 2, s.Free())
	release, ok := s.Acquire("A", video.OutputDASH, func() {})
	require.True(t, ok)
	require.Equal(t, 1, s.Free())
	release()
	release() // twice is once
	require.Equal(t, 2, s.Free())
}
