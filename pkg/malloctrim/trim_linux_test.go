//go:build linux

package malloctrim_test

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/ebitengine/purego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/malloctrim"
)

// resident is the process's resident memory in bytes.
func resident(t *testing.T) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/statm")
	require.NoError(t, err)
	pages, err := strconv.ParseInt(strings.Fields(string(b))[1], 10, 64)
	require.NoError(t, err)
	return pages * int64(os.Getpagesize())
}

// The worker's case in small: memory malloc'd and freed stays resident
// while a few allocations among it live on, and Trim gives it back.
func TestTrimReturnsTheFreedMemoryAnArenaKeeps(t *testing.T) {
	if _, err := malloctrim.Trim(); errors.Is(err, malloctrim.ErrUnsupported) {
		t.Skipf("no glibc here: %v", err)
	}
	libc, err := purego.Dlopen("libc.so.6", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	require.NoError(t, err)
	var (
		malloc func(size uintptr) uintptr
		free   func(p uintptr)
		memset func(p uintptr, c int32, n uintptr) uintptr
	)
	purego.RegisterLibFunc(&malloc, libc, "malloc")
	purego.RegisterLibFunc(&free, libc, "free")
	purego.RegisterLibFunc(&memset, libc, "memset")

	// One thread, so one arena. 64 KiB is under malloc's mmap threshold:
	// the chunks come from the arena, where a freed one is not unmapped.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const size, count = 64 << 10, 4096 // 256 MiB
	chunks := make([]uintptr, count)
	for i := range chunks {
		chunks[i] = malloc(size)
		require.NotZero(t, chunks[i])
		memset(chunks[i], 1, size)
	}
	var kept []uintptr
	for i, p := range chunks {
		if i%16 == 15 {
			kept = append(kept, p) // what outlives the conversion
			continue
		}
		free(p)
	}
	defer func() {
		for _, p := range kept {
			free(p)
		}
	}()
	held := resident(t)

	released, err := malloctrim.Trim()
	require.NoError(t, err)
	assert.True(t, released, "malloc_trim reports memory released")
	after := resident(t)
	assert.Greater(t, held-after, int64(160<<20),
		"of the 240 MiB freed, most leaves the process: %d MiB resident before, %d MiB after", held>>20, after>>20)
}
