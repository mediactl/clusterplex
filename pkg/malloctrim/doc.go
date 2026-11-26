// Package malloctrim gives the C allocator's free memory back to the system.
//
// The workers run FFmpeg in process, and what its threads allocate comes
// from glibc's malloc: one arena a thread. An arena only shrinks from its
// top, so once a conversion's buffers are freed whatever small allocation
// outlives it keeps the pages below it resident. The CPU pool's worker held
// 1,030 MiB with nothing to convert (talos, 2026-11-25: 967 MiB of it in 22
// arenas, 23 MiB of Go heap in use, the last of its 27 conversions ended the
// day before). malloc_trim releases the free pages inside every arena.
package malloctrim

import "errors"

// ErrUnsupported is Trim's answer where there is no malloc_trim to call: a
// C library that is not glibc, or a platform that is not Linux.
var ErrUnsupported = errors.New("malloctrim: no malloc_trim in this process")
