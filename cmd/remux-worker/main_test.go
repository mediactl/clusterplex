package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// v returns FFmpeg's three library versions with libavcodec's major set.
func v(avcodecMajor uint32) func() (uint32, uint32, uint32) {
	return func() (uint32, uint32, uint32) { return 60 << 16, avcodecMajor << 16, 62 << 16 }
}

func api(n int) func() int { return func() int { return n } }

func TestTheWorkerRefusesAnOldShim(t *testing.T) {
	err := checkRuntime(v(63), api(0), 1)
	require.ErrorContains(t, err, "shim API")

	require.NoError(t, checkRuntime(v(63), api(1), 1))

	err = checkRuntime(v(62), api(1), 1)
	require.ErrorContains(t, err, "libavcodec")

	// The pinned ffgo forces keyframes through API 2 (v0.0.0-clustarr.14).
	require.ErrorContains(t, checkRuntime(v(63), api(1), minShimAPI), "shim API")
	require.NoError(t, checkRuntime(v(63), api(2), minShimAPI))
}
