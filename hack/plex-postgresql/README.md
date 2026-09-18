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

## 0002 — intern declared types so lookups cannot read freed memory

A genuine use-after-free on the hottest path in the shim.

`rust_decltype_cache_lookup` and `rust_decltype_cache_lookup_alias` take a read
lock on a `HashMap<String, CString>`, return a raw pointer into the stored
`CString`, and release the lock on the way out. The caller then reads those
bytes with no lock held.

`rust_decltype_cache_insert` used `HashMap::insert`, which drops the previous
value for a key and frees its buffer. So re-inserting a key freed bytes that
another thread was part way through reading. Every column access does a
lookup, so the window is wide open.

The values are now interned with `Box::leak`, and an insert that would change
a value leaves the old allocation alone instead of freeing it. Nothing can
free bytes a reader still holds. It costs one allocation per column, and only
a value that actually changes leaks anything.

The sibling `OID_TABLE_CACHE` is fine as it stands: it inserts with
`entry().or_insert()`, so a stored value is never replaced or dropped.

**This did not fix the crash.** It is a real bug on the same code path and
worth carrying, but Plex still dies in `column_type` reading
`SELECT version FROM schema_migrations`. So there is at least one more
unsynchronised access, and this one was not it.

## What is known about the remaining crash

- It is a race. The row it dies on moves between runs — 91, 134, 259, 283 —
  and it presents as both SIGSEGV and a `DB::Exception` thrown out of Plex's
  SQLite layer, which is what reading recycled memory looks like.
- It is always in `column_type`, on a 446-row result.
- Two threads interleave in the `[EXC_CONTEXT]` ring: one iterating
  `schema_migrations`, one writing `statistics_bandwidth`. Different statements
  and different database handles.
- The result-clearing path is *not* the culprit. `rust_stmt_clear_result` takes
  no lock itself, but every caller holds the statement mutex, and there is no C
  caller — the tree is pure Rust with legacy headers.
- `column_type_impl` reads `pg_stmt.result`, `cached_result` and `pg_sql`, and
  may call `ensure_pg_result_for_metadata`, all *before* taking the statement
  mutex. `ensure_pg_result_for_metadata` then mutates the statement unlocked.
  That is a real race, but it needs a null result to trigger and ours is not
  null, so it is not this crash.
- `column_type_impl` resolves the statement with `pg_find_any_stmt` and uses
  the pointer without taking a reference — `rust_stmt_find_any` returns the raw
  pointer and drops the registry lock without touching `ref_count`, so
  `pg_stmt_free`'s "refuse to free while referenced" guard cannot protect it.
  **Ruled out for this crash**: the statement it dies on is never freed. Grep
  the log for the crashing `stmt=` pointer and there is no matching
  `pg_stmt_free` or `pg_stmt_unref` at all. Still a latent bug, just not this
  one.
- **Ruled out: a shared `PGconn`.** libpq forbids using one connection from two
  threads, but the two threads run on different ones —
  `STEP READ/WRITE ... exec_conn=` shows a distinct connection per thread.
- **Ruled out: two threads on one statement.** Summarising the phase ring by
  thread and statement shows the crashing thread owns its statement
  exclusively, alternating `column_type` and `column_text`; the other thread is
  on entirely different statements.

So the corruption is not on the statement, the connection or the statement's
lifetime. It has to be global state that both threads reach. The declared-type
cache was one such, and it was genuinely broken, but fixing it was not enough —
so look for the next thing that hands a raw pointer out of a shared structure
and then releases the lock. The pattern to grep for is a function returning
`*const c_char` that takes a lock, calls `.as_ptr()` on something it does not
own, and returns.

Two that were checked and are fine: `OID_TABLE_CACHE` inserts with
`entry().or_insert()` so values are never replaced, and a `HashMap` rehash
moves the `CString` struct but not the heap buffer `as_ptr()` points at;
`rust_query_cache_release` only decrements a refcount and frees nothing.
- Statement mutexes are `std::sync::Mutex`, which is **not** recursive, so any
  fix that adds a lock has to check every caller first. Connection mutexes are
  pthread recursive; the two are easy to confuse.

None of the shim's own knobs avoid it: `DISABLE_STREAMING`, `DISABLE_POOL`,
`POOL_SIZE=1`, `DISABLE_STMT_CACHE`, `DISABLE_QUERY_CACHE` and
`DISABLE_PREPARED` were each tried.
