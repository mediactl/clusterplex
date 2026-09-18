# Patches to the PostgreSQL shim

Upstream is `cgnl/plex-postgresql`, pinned in the Dockerfile. The image builds
the shim from source, so we carry local changes as patches here rather than
forking, the way `hack/litefs` used to. They are applied in filename order and
the build fails if one does not apply, so a version bump cannot silently drop
a fix.

Anything here is a candidate to send upstream. Bear in mind that the last
maintainer commit was 1 April 2026 and open pull requests have gone unanswered,
so assume we carry these ourselves.

## 0001 — collect backtraces with the DWARF unwinder

The shim prints a backtrace when it catches a fatal exception, but on Linux it
collected frames by walking the `%rbp` chain. Release builds are compiled with
`-fomit-frame-pointer`, so there is no chain to walk: every backtrace came out
as `[Stack trace unavailable]`.

This matters because that is the exact state the two open upstream crash
reports are stuck in — [#17](https://github.com/cgnl/plex-postgresql/issues/17)
and [#26](https://github.com/cgnl/plex-postgresql/issues/26) both say the stack
trace was unavailable, which is why neither has been diagnosed.

The patch collects frames with `_Unwind_Backtrace` from libgcc, which uses the
DWARF call-frame information and does not need frame pointers. `libgcc_s.so.1`
is already linked and shipped. The old walk is kept as a fallback for frames
with no unwind information. Each frame is written to stderr as it is found,
because the walk runs on a stack that has often already been corrupted and can
fault part way through; an address printed before that is still usable.

**It does not work, and that is the finding.** The unwinder is linked — the
library carries `_Unwind_Backtrace@GCC_3.3` as an undefined symbol — and the
header line is printed at every crash, but no frames ever follow. So there is
no usable call-frame information to unwind with in this process.

That rules out stack-based debugging here, which is worth knowing before
anyone spends more time on it: this is why upstream's two crash reports say
"stack trace unavailable", and it is not something a build flag on our side
fixes. Plex is closed-source and compiled without unwind tables, and the shim
sits under it as an `LD_PRELOAD` interposer.

Bisecting the race will have to use the shim's own instrumentation instead.
`PLEX_PG_ENABLE_EXCEPTION_CATCHER=1` with `PLEX_PG_EXCEPTION_VERBOSE=1` already
prints `[EXC_CONTEXT]` — a ring of the last ~48 operations with thread id,
statement and SQL for each — which is what identified the interleaving in the
first place.

The patch is kept because it costs nothing, falls back cleanly, and would start
producing frames if Plex ever ships unwind tables.
