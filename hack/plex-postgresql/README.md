# The PostgreSQL shim

The image builds the shim from source, from **our fork**,
[`mediactl/plex-postgresql`](https://github.com/mediactl/plex-postgresql),
pinned in the Dockerfile by tag. Upstream is `cgnl/plex-postgresql`; the last
maintainer commit was 1 April 2026 and open pull requests have gone unanswered,
so assume we carry everything ourselves.

Fixes go to the fork with a test, and come back here as a tag. There is no
patch stack any more — it was replaced by the fork once the fixes stopped being
one-liners, and `0002` below is in the fork instead, in a better form.

This directory holds what the image needs beside the shim — the vendored schema
dumps and upstream's init script — and the notes below, which are the record of
what has been established about the shim's behaviour. Read them before spending
a day on something already ruled out.

To work on the shim: clone the fork, change it, `cargo test --lib` in
`rust/plex-pg-core`, tag, and bump `PLEX_PG_REF`. `PLEX_PG_REPO` is an
`ARG` too, so a branch can be tried without editing the Dockerfile.

## Which upstream release to build, and why not an older one

`v1.3.17`, pinned in the Dockerfile. It is the first published release whose
image actually loads the shim.

`Dockerfile.standalone` injects the shim by rewriting the s6 run script, and
until `v1.3.17` its patterns were written for the **linuxserver** image — user
`abc`, binary path in double quotes. `plexinc/pms-docker` ends its run script
with

```sh
exec s6-setuidgid plex /usr/lib/plexmediaserver/Plex\ Media\ Server
```

— user `plex`, path escaped rather than quoted. Neither pattern matched,
neither substitution failed, and the image shipped with no `LD_PRELOAD` at all.
The init script noticed at runtime and only warned:

```
PostgreSQL shim library found: /usr/local/lib/plex-postgresql/db_interpose_pg.so
WARNING: Plex run script missing LD_PRELOAD - shim may not load!
```

`v1.3.17` adds a third pattern matching the `plexinc` form, and its image does
start Plex through `plex-with-shim.sh`. Reading the last line of
`/etc/services.d/plex/run` out of each published image:

| tag | last line of the run script | shim |
| --- | --- | --- |
| `v1.0.0` | `... plex /usr/lib/plexmediaserver/Plex\ Media\ Server` | no |
| `v1.2.0` | `... plex /usr/lib/plexmediaserver/Plex\ Media\ Server` | no |
| `v1.3.0` | `... plex /usr/lib/plexmediaserver/Plex\ Media\ Server` | no |
| `v1.3.17` | `... plex /usr/local/lib/plex-postgresql/plex-with-shim.sh` | yes |
| `latest` | `... plex /usr/local/lib/plex-postgresql/plex-with-shim.sh` | yes |

**This is what invalidated the release bisect that once pinned us to `v1.2.0`.**
Running one published image per release against a fresh `postgres:15-alpine`
compared releases that never loaded the shim against releases that did.
`v1.2.0` "came up healthy" because it was running Plex on its ordinary SQLite
file: its container had a 1MB `com.plexapp.plugins.library.db` with a live WAL
and **zero** connections in `pg_stat_activity`. Check it that way before
trusting any future comparison — a healthy container proves nothing on its own.

So `v1.3.17` is not merely newer. It is the only published configuration in
which upstream exercises their own interposer.

To get a reference out of an older tag, replace its entrypoint with one that
repairs the injection before handing over to s6: write a wrapper exporting
`LD_PRELOAD` and `LD_LIBRARY_PATH` and exec'ing the real binary, point the run
script at it, then `exec /init`. Done that way, `v1.2.0` gets through session
capture and dies on the first forward migration — the failure described in the
next section.

What `v1.3.17` gives us beyond the injection: it builds the `subreaper` the
manager depends on, so `subreaper.c` is no longer carried here; it ships
`seed_data.sql` and `seed_shadow_table_from_pg.py`, which seeds the shadow's
`preferences` table from PostgreSQL; and its two schema dumps agree with each
other about `metadata_items.user_square_art_url`, where `v1.2.0`'s did not.

## The dump does not record one of the migrations it already contains

This is what stopped Plex booting, and it is a data bug in `plex_schema.sql`.

The dump's `metadata_items` already has `user_square_art_url`, the column
migration `202510021115` adds. Its `schema_migrations` block — 445 rows — does
not list `202510021115`. Plex reads that table to decide what is left to apply,
finds the migration outstanding, and sets out to run it.

To run a migration Plex captures all twenty of its library sessions. In our
cluster it captured nineteen and stopped:

```
Running migrations. (EPG 0)
Captured session 0.
...
Captured session 18.
```

The twentieth belonged to Plex's statistics thread, which had started
concurrently, prepared `SELECT ... FROM statistics_bandwidth`, and blocked
inside the shim between `sqlite3_bind_parameter_index` and the bind that
follows it. It never gave the session back. Every thread in the process ends up
in `futex_wait`, PostgreSQL shows no active query and no ungranted lock, and
Plex answers 503 forever with nothing further in either log.

On plain SQLite the same migration simply adds the column a second time and
succeeds, which is why upstream has not noticed: every published image before
`v1.3.17` ran Plex that way, so nothing they shipped could have hit it.

The fix is at the end of `schema/plex_schema.sql`: one idempotent `INSERT`
recording `202510021115` as applied, with the three null columns a stock Plex
1.43.0.10492 writes when it runs the migration itself. That list came from a
published image running Plex on SQLite, whose `schema_migrations` differs from
the dump's by exactly this one row.

`pkg/plexdb/schema_test.go` guards it, so re-vendoring a dump that carries the
column without the row fails the build rather than hanging a pod.

### Still inconsistent between the two dumps

`sqlite_schema.sql` still trails `plex_schema.sql`. Comparing column sets at
`v1.3.17`, the shadow is missing `metadata_items.subtype` and
`media_stream_settings.created_at`/`updated_at`. (`search_vector`, `title_fts`
and `schema_migrations.id` are PostgreSQL-only by design, and `v1.3.17` closed
the `user_square_art_url` gap that `v1.2.0` had.) Recording the migration means
Plex will not add the rest to the shadow either, so a query naming one of those
columns may fail to prepare against it. Left alone deliberately — no failure
has been observed from it yet, and one change at a time.

Re-run the comparison after any re-vendoring: parse `CREATE TABLE plex.<name>`
out of `plex_schema.sql` and the quoted column names out of `sqlite_schema.sql`,
and diff the sets per table.

## 0001 — collect backtraces with the DWARF unwinder (removed)

Removed, not merely unhelpful. It replaced `collect_frames` with
`_Unwind_Backtrace`, which produced no frames because nothing in the process
carries unwind information — and `platform_print_backtrace` is called from
ordinary paths as well as fatal ones (`first_execute/connection.rs`,
`step_write_utils/connection.rs`, `ring_tracker.rs`), so it was running the
unwinder during normal operation. It was not the cause of our crash, which was
tested by removing it, but running an unwinder that cannot work on live code
paths is not something to keep for a diagnostic that never produced output.

The finding it leaves behind is the useful part: **stack-based debugging is not
available in this process**, which is why upstream's crash reports all say
"stack trace unavailable" and why this cannot be chased with a backtrace.

## 0001 — what it was

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

## What it took to get Plex to start

Eight defects, each hiding the next. They are listed in the order they had to
be fixed, because that is the order they appear if anyone repeats this.

1. **The dump did not record migration `202510021115`** though it already had
   the column. Plex set out to re-apply it — see the section above.
2. **The worker delegation slot** was shared between callers, so a prepare
   delegated to the 8MB worker could have its completion cleared by the next
   caller. This was the hang in "Running migrations".
3. **Passthrough metadata calls answered from PostgreSQL.** Plex keeps its
   statistics in an in-memory database the shim does not redirect;
   `last_insert_rowid` answered from the shim's global PostgreSQL row id, so
   Plex's insert-then-read loop never converged and retried once a second for
   ever.
4. **SIGCHLD was forced to SIG_IGN**, so `wait()` failed with ECHILD and Plex
   could not manage the Python processes its plug-ins run in. It reached
   "Media provider refresh complete" and stopped. Disabled from our side with
   `PLEX_PG_DISABLE_SIGCHLD_IGNORE=1`; we run Plex under a subreaper and its
   CrashUploader is a no-op, so we never needed it.
5. **`vfork` was interposed.** A vfork child runs on the parent's stack with
   the parent's thread suspended and must never return from the frame that
   called vfork — which is exactly what an interposer makes it do. The plug-in
   process started and ran; the parent came back to a clobbered stack and died
   with an instruction pointer outside every loaded module.
6. **`boost::locale::util::create_simple_converter` was interposed with the
   wrong ABI.** It returns a class type, so RDI is the hidden result pointer
   and the encoding arrives in RSI; declared as `fn(*mut u8) -> *mut c_void`
   the wrapper handed Boost its own return slot as the encoding name. It was
   also the only C++ symbol the shim exported, shadowing Plex's own
   ICU-backed `libboost_locale.so`. Replaced by an assembly hook that does
   what AArch64 has always done: redirect the ASCII charset to UTF-8.
7. **The init script stripped the dashes out of the machine identifier**
   (`tr -d '-'`), leaving 32 bare hex characters where Plex parses a UUID:
   `std::domain_error: Invalid uuid length`. Fixed in our vendored copy.
8. **The worker thread did not block signals.** A library thread must never be
   a candidate for the process's asynchronous signals; Plex handles its own on
   a dedicated `sigwait` thread, and when the kernel picked ours instead it
   treated the signal as fatal — `Received unexpected async signal 17`,
   moments after the server started answering requests.

With those in place Plex starts, answers `/identity` with a real
MediaContainer, runs its plug-ins and holds ~35 PostgreSQL connections.

### Fixed: the crash on `GET /` was a NULL column reported as TEXT

`sqlite3_column_type` never returned SQLITE_NULL. For a NULL value it
reported the type derived from the PostgreSQL OID instead, so Plex could not
tell "no value" from "empty value". Upstream added that to stop SOCI throwing
`std::bad_cast` when the holder it allocated from `sqlite3_column_decltype`
did not match; real SQLite returns SQLITE_NULL and SOCI copes with it, so a
bad_cast means the decltype is wrong and that is the bug to fix.

What it cost: the plug-in framework issues `GET /`, which runs a LEFT JOIN
over `plugins` and `plugin_prefixes`, and `plugin_prefixes.prefix` is NULL for
every plugin without a prefix. Told the column was TEXT, Plex read an empty
string and parsed it as a path -- it looks for the second `/`, does not find
one, and builds a substring at the resulting negative offset:

    libc++abi: terminating with uncaught exception of type
    std::out_of_range: basic_string

one millisecond after the request. Fixed in `v1.3.17-clusterplex.10`; `GET /`
now answers 200, including the two concurrent ones the plug-ins issue at
startup.

The throw site was found with the `__cxa_throw` backtrace added in the same
tag (`PLEX_PG_EXCEPTION_BACKTRACE=1`), then read off the disassembly. Reach
for that first next time: everything before it in this file was inference, and
several of those inferences were wrong.

### Still open: `Invalid uuid length` once plex.tv is reachable

With plex.tv resolving normally, Plex serves for a few seconds and then throws,
uncaught, about twenty milliseconds after a 200 from `plex.tv/api/v2/features`:

    libc++abi: terminating with uncaught exception of type
    std::domain_error: Invalid uuid length

**The parser accepts one length and one only.** From the throw site itself --
`0x1fccba` is the message; `0x2a31cd`, which earlier notes pointed at, is the
neighbouring *"Character at index 8 must be a '-'"*:

    11497ad:  cmp  $0x24,%rsi       ; length == 36?
    11497b1:  jne  114992c          ; no -> throw "Invalid uuid length"
    11497ba:  cmpb $0x2d,0x8(%rdi)  ; then dashes at 8, 13, 18, 23

Not 32, not 38, which boost normally also takes. Exactly 36.

**What feeds it.** The function two frames up holds the literals `//feature`
and `uuid`: it runs that XPath over an XML document, collects each selected
element's `uuid` attribute into a list, and parses every entry. The loop reads
a libc++ string per node and passes (data, size) straight in, so a missing
attribute arrives as an empty string -- length 0, and the throw.

And the documents differ in exactly that way:

| document | `<feature>` elements | carrying `uuid=` |
| --- | --- | --- |
| `/api/v2/features` (anonymous or claimed) | 61 / 174 | all of them |
| `/api/v2/user?includeSubscriptions=1&includeProviders=1` | 174 | **none** |

The user document's features carry `id` only.

**The control that matters, and that earlier attempts got wrong.** Run the
pinned Plex on its own -- same version, same auth token, same plex.tv, its own
fresh SQLite, no `LD_PRELOAD`, no PostgreSQL, no manager:

```sh
docker run -d --name plexbare -v "$CFG:/config" \
  -e "PLEX_MEDIA_SERVER_APPLICATION_SUPPORT_DIR=/config/Library/Application Support" \
  -e "LD_LIBRARY_PATH=/usr/lib/plexmediaserver/lib" \
  --entrypoint "/usr/lib/plexmediaserver/Plex Media Server" \
  ghcr.io/mediactl/cluster-plex:<tag>
```

It runs for as long as you leave it, fetches `/api/v2/user` repeatedly, and
throws nothing -- MyPlex gets as far as "updating with 24 access tokens". So
the user document is **not** inherently fatal, the pinned Plex is **not** too
old for today's plex.tv, and the fault is on our side.

Do not repeat the invalid version of this control: starting Plex without the
shim *inside our image* reuses the shadow SQLite the entrypoint builds, which
is deliberately incomplete (`no such module: spellfix1`), so Plex dies in
`DatabaseFixups` instead and proves nothing.

**The difference to chase.** The standalone asks for
`/api/v2/server/users/features` and gets 200. Ours never asks for it at all;
it asks for `/api/v2/server/users` and `/api/v2/server/users/subscriptions`
instead. If the features list normally comes from the server endpoint and only
falls back to the user document when that is missing, then the fallback is the
path that throws, and the question becomes why our server never makes that
request -- `PublishServerOnPlexOnlineKey` is forced to `0` here, so plex.tv
never registers this server.

**Ruled out, each by testing it:** the response body (5182 bytes, pure ASCII,
every uuid 36); the charset and the `create_simple_converter` redirect;
`PlexOnlineToken=""` left behind by a claim that failed while plex.tv was
pinned to loopback; `ProcessedMachineIdentifier` (40 characters, and not
derived from the MachineIdentifier this repo forces); 24-character client
identifiers in `plex.devices`; and claiming the server, which works -- the
token can be exchanged out of band and written into `Preferences.xml` -- but
does not stop the crash.

### There is no configuration you can actually use yet

`--block-plex-tv` gives a server that comes up `1/1` and serves, which is
enough to exercise everything else here, and it is a real setting rather than
the `hostAliases` patch that used to stand in for it. But it only survives
while nobody uses it. Sign in through the web client and the server dies on
the next request:

    libc++abi: terminating with uncaught exception of type
    UnauthorizedException: HTTP status code 401

`/media/providers` on the server is a proxy for `https://plex.tv/media/providers`.
With plex.tv dropped the background refresh fails harmlessly --
`[MediaProviderManager/Response::fetch] failed to complete query (408)` -- but
the request the signed-in web client makes turns the same unreachable plex.tv
into an uncaught exception. In the log `GET /media/providers` never completes
and the crash handler runs 95ms later, with no outbound request logged at all:
it throws inside the handler.

So the two symptoms are one problem wearing two faces. This configuration
sends Plex down plex.tv paths it cannot finish: reachable, it dies parsing
feature uuids about fifteen seconds in, with nobody touching it; blocked, it
dies the moment somebody signs in. Fixing the uuid crash is the way out --
blocking is somewhere to stand while debugging, not somewhere to run.

The pod does at least recover now. The manager gives up after three missed
health checks and exits so Kubernetes restarts the container, rather than
leaving it `0/1` for ever with a dead Plex inside it.

## Races fixed in the fork

All of these are reachable from the overlap that happens on every boot: Plex
runs its migration check and its statistics fixups on different threads at the
same time, both through the shim. Together they presented as Plex hanging in
"Running migrations" — 503 to everything, every thread in `futex_wait`,
PostgreSQL showing no active query and no ungranted lock — or, when the timing
shifted, as a crash in the fixup thread instead.

- **The worker delegation slot.** A prepare on a small stack is delegated to
  the shim's 8MB worker thread through one global `worker_request`. The caller
  waits for its answer on `worker_cond_response` with `worker_mutex` released,
  which is exactly when a second caller can take that mutex and overwrite the
  slot — clearing a `work_done` its owner has not read yet. The owner then
  waits for a response that has already been signalled, or reads the other
  caller's statement. Serialised end to end now; the regression test strands
  8 of 8 callers without the fix and passes 1600 delegations in 0.06s with it.
- **`rust_worker_init` was not idempotent**, so two threads finding no worker
  both created one: two workers on a single request slot, and the first thread
  handle orphaned where cleanup could never join it.
- **The declared-type cache replaced published entries.** Lookups return a
  pointer into the stored `CString` and then drop the read guard; those
  pointers reach Plex as `sqlite3_column_decltype()` results and stay live
  until the statement is finalized. Replacing freed them underneath a reader.
  First publication now wins. (This is what the old `0002` patch fixed by
  interning with `Box::leak`; `or_insert` is the better answer and leaks
  nothing.)
- **The TLS key used its own value as a success flag**, but `pthread_key_t` is
  an index and 0 is an ordinary key — on glibc, the first one handed out. A
  failed `pthread_key_create` therefore looked like key 0, and the per-thread
  reentrancy guards silently became process-wide.
- **Pool slots could be claimed after the reaper had sampled them**, handing
  out a connection that was being `PQfinish`ed. Claims re-read the connection
  pointer under the claim.
- **`rust_stmt_cache_lookup` returned a borrowed pointer** after dropping the
  cache mutex. Callers hold that name across `PQprepare` and `PQexecPrepared`,
  during which another thread on the same pooled connection can re-prepare,
  evict or drop the entry. The name is copied into thread-local storage now,
  which keeps the `const char *` ABI and gives callers a lifetime they can
  rely on.

The pattern in all six: something hands a raw pointer out of a shared
structure, or shares a single slot, and then releases the lock. When looking
for the next one, grep for a function that takes a lock, calls `.as_ptr()` on
something it does not own, and returns.

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
`POOL_SIZE=1`, `DISABLE_STMT_CACHE`, `DISABLE_QUERY_CACHE`, `DISABLE_PREPARED`
and `LEAK_STMTS` were each tried. The last is worth noting: it makes the shim
never free a statement, which rules out statement lifetime as the cause even
though the logs put a `pg_stmt_free` immediately before one of the crashes.

## "It is not our build" — withdrawn

This section used to say that upstream's own image failed the same way and so
the shim was broken for everyone. That reading was wrong, and it is left here
corrected rather than deleted because it sent the investigation the wrong way
for a long time.

What was observed is real: `ghcr.io/cgnl/plex-postgresql-plexinc:latest`, run
with its own entrypoint against a fresh `postgres:15-alpine`, crashes **80
times in two minutes** with

    libc++abi: terminating with uncaught exception of type
      soci::soci_error: sqlite3_statement_backend::loadOne: not an error

and never serves a request, staying "Up" only because s6 restarts Plex
underneath it.

What it does not show is anything about the shim. That container was running
Plex on its own SQLite file — see the first section — so the crash is stock
Plex choking on the SQLite database the init script pre-seeds, not the
interposer. Nothing there implicates or exonerates our build.

It is still worth reporting, alongside the `sed` that never matches, on
[#17](https://github.com/cgnl/plex-postgresql/issues/17) and
[#26](https://github.com/cgnl/plex-postgresql/issues/26), neither of which has
a maintainer response.
