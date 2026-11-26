//go:build linux

package malloctrim

import (
	"fmt"
	"sync"

	"github.com/ebitengine/purego"
)

var (
	load sync.Once
	trim func(pad uintptr) int32
	fail error
)

// Trim releases the free memory glibc's malloc holds, in every arena, and
// reports whether any went back to the system. It is safe beside running
// allocations: an arena is locked only while it is walked.
func Trim() (bool, error) {
	load.Do(func() {
		// The process's own C library, the one FFmpeg allocates from: it is
		// loaded already, and this takes a handle on it.
		libc, err := purego.Dlopen("libc.so.6", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			fail = fmt.Errorf("%w: %w", ErrUnsupported, err)
			return
		}
		sym, err := purego.Dlsym(libc, "malloc_trim")
		if err != nil {
			fail = fmt.Errorf("%w: %w", ErrUnsupported, err)
			return
		}
		purego.RegisterFunc(&trim, sym)
	})
	if fail != nil {
		return false, fail
	}
	return trim(0) != 0, nil
}
