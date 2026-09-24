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

`pkg/plex/db/schema_test.go` guards it, so re-vendoring a dump that carries the
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

## What a Plex version bump actually costs

`ARG VERSION` in the Dockerfile was pinned with a note saying a newer server
rebuilds its full-text tables and dies on the `fts4` DDL. That was right, and
it stayed right through `1.43.4.10903`. What follows is how to check it, and
what the check found, because "pinned, do not touch" is not a plan.

**The migrations are the cheap half.** Boot the candidate on plain SQLite — no
shim, no PostgreSQL, `docker run plexinc/pms-docker:<version>` — let it settle,
and read `schema_migrations` out of the library it builds. Diff that against
the `COPY plex.schema_migrations` block in `plex_schema.sql` plus the one row
appended after it. Do not try to read the list out of the binary: the migration
identifiers are not stored as plain strings, and `strings` finds only the ones
from 2024 and earlier, which looks like an answer and is not.

`1.43.0.10492` → `1.43.4.10903` leaves exactly two outstanding, `202601121053`
and `202608120900`, and neither adds a table or a column the dump does not
already have. `202608120900` carries `rollback_sql = 'select 1'` with
`optimize_on_rollback = 1`, the same shape as `202505261219` — Plex's marker
for an index rebuild.

**The rebuild is the expensive half, and it is not in the migration list.**
Plex concludes the library was written by an older server and rebuilds the
full-text index: sixteen `DROP TRIGGER`s, four `DROP TABLE`s, two
`CREATE VIRTUAL TABLE ... USING fts4`, and eight `CREATE TRIGGER`s. It then
vacuums once the migrations are done. The only way to see this is to run the
candidate against a fresh PostgreSQL loaded from the dump with
`PLEX_PG_LOG_LEVEL=DEBUG` and read the shim's log. Sixty-eight errors, and the
server exits before it serves:

    Unable to set up server: sqlite3_statement_backend::loadOne: not an error

Three separate defects, fixed in `v1.3.17-clusterplex.12` and `.13`:

- **`fts4` DDL reached PostgreSQL untranslated.** `rewrite_virtual_tables`
  handled `fts5` and `rtree`; Plex writes `fts4`. The drops were worse than the
  creates: `DROP TABLE IF EXISTS fts4_metadata_titles` answered
  `"fts4_metadata_titles" is not a table`, because under the shim those four
  names are **views** the dump provides over the real tables. Had the syntax
  been accepted it would have deleted the compatibility layer. The whole
  rebuild is now `SELECT 1`.
- **`VACUUM` ran against the shadow SQLite.** It is on the skip list, but a
  skipped statement only gets the `is_pg = 3` no-op when it also looks like a
  read or a write, and `VACUUM` is neither. It reached the shadow inside the
  transaction the migrations were holding open, where SQLite refuses it.
  `BEGIN` and `COMMIT` take the same path and hid the gap for as long as they
  did because they succeed against the shadow.
- **`sqlite_stat1` went to PostgreSQL.** It holds what `ANALYZE` collected for
  SQLite's query planner, and belongs to SQLite the way `sqlite_master` does —
  but it was missing from the passthrough list, so Plex's read of it became
  `relation "main.sqlite_stat1" does not exist` on every start. Nothing here
  defines it, neither the dump nor the shadow schema. It goes to the shadow
  now, which answers "no such table", which is what Plex sees on any library
  nothing has analyzed. Not fatal, but it was the last line in the log.

**Always run the control.** The first time, the failing run proves nothing on
its own — the harness could be at fault. Build the *current* pin from the same
Dockerfile, run it against the same rig, and confirm it comes up. `1.43.0`
does, with one error in the shim log and no `CREATE VIRTUAL TABLE` at all,
which is what makes the newer server's 68 errors a regression rather than a
property of the test. This repo has been caught by a bad control twice; see
"It is not our build" below.

**Upstream will not do this for you.** `cgnl/plex-postgresql` releases are
automated base-image bumps — every changelog entry from `1.3.9` to `1.3.18`
reads "Updated upstream base Docker images", written by
`scripts/check-upstream-updates.py` when the `plexinc/pms-docker:latest` digest
moves. The schema dumps do not move with them: at `v1.3.18` all four are
identical to what is vendored here apart from our own two additions. Taking a
newer upstream tag buys nothing for a Plex bump.

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

### Fixed: interposing `__cxa_throw` broke every catch in Plex

This is what was behind the crash on plex.tv, and behind two others that
looked nothing like it.

The shim interposed `__cxa_throw`, which puts a frame belonging to this
library between every `throw` in Plex and the `catch` that was meant to
handle it. Plex does not survive that: **the first exception the process
throws is fatal, whatever it was**. Plex throws and catches routinely, so the
symptom depended entirely on which exception happened to come first -- three
unrelated looking crashes, every one of them something Plex handles perfectly
well on its own:

    std::out_of_range: basic_string
    std::domain_error: Invalid uuid length
    UnauthorizedException: HTTP status code 401

One request reproduces it in about a minute, with no Kubernetes, no cluster
and no manager:

| `GET /media/providers`, no token | result |
| --- | --- |
| plain Plex, own SQLite | 401, keeps running |
| shim + fresh PostgreSQL | terminates, uncaught |
| shim without the hook | 401, keeps running |

Fixed in `v1.3.17-clusterplex.11`. With it the server comes up `1/1` with
plex.tv reachable, reaches `Mapped - Publishing`, opens the pubsub
EventSource connection, and fetches `/api/v2/server/users/features` -- none of
which it had ever managed before.

**Why it took so long to see.** Two earlier investigations went past it:

- A throw/catch test built against **libstdc++** passes with the hook loaded,
  which is what cleared it the first time. libstdc++ and libgcc are a matched
  pair and forwarding between them is harmless. Plex is libc++ with LLVM's
  unwinder, and that is the pairing that breaks. A control has to use the
  runtime the program actually uses.
- Every symptom pointed somewhere else, plausibly. The `Invalid uuid length`
  crash really does follow a document whose `<feature>` elements carry no
  `uuid` -- but Plex only asked for that document because it had not reached
  `Mapped - Publishing`, and it had not reached it because it had already
  died. The whole chain was downstream of the first throw.

The hook, and the `__cxa_throw` backtrace that depends on it, are kept behind
an `exception-hook` Cargo feature, off by default. That backtrace is what
found the NULL column-type bug and is worth having for the next one; turning
it on means accepting that Plex will die on its first exception.

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

## Fixed: the pool probed dead threads with `pthread_kill`, which segfaults on musl

Plex on every pod died within minutes of its first playback, in
`sqlite3_prepare_v2` or `sqlite3_step`, and nothing logged it. Fixed in
`v1.3.17-clusterplex.19`.

**Why it was silent.** Plex installs its own SIGSEGV handler and re-raises
the signal, so the kernel's usual `segfault at` line never appears; with crash
reporting disabled no minidump is written; the shim's handler
(`PLEX_PG_ENABLE_SIGNAL_LOG=1`) is installed with `sa_flags = 0` and never got
to run; PostgreSQL saw nothing; Plex's own log stops mid-request. The manager
only noticed the listener was gone.

**How it was found.** Three things, in order:

1. `sysctl -w kernel.print-fatal-signals=1` on the kind node. Every death then
   logged `Plex Media Server: PMS ReqHandler: potentially unexpected fatal
   signal 11` -- and that wording means the signal was delivered with its
   *default* action, i.e. whatever handler was installed could not run.
2. Core dumps were already being written and nobody had looked: the host's
   `core_pattern` is `core`, Plex's core limit is unlimited, and Plex's working
   directory is `/`, so each death left `/core.<pid>` (~150 MB) in the pod's
   root filesystem. They survive an in-place Plex restart and are lost when
   the pod is recreated.
3. Host `gdb` on the core through the container's root:
   `gdb -c /proc/<manager pid>/root/core.<pid> "/proc/<manager pid>/root/usr/lib/plexmediaserver/Plex Media Server"`
   with `set sysroot /proc/<manager pid>/root`. Plex is stripped and gdb
   cannot walk musl's link map, so frames in Plex itself are `??`, but the shim
   keeps its symbol table and the shim, libpq, soci and libc++ frames resolve.
   Attaching gdb to the live process from the host does not work: it cannot
   see the threads across the PID namespace.

Three cores from three pods, one stack:

```
#0  ld-musl-x86_64.so.1   lock cmpxchg %edx,(%rdi)
#1  pthread_kill
#2  plex_pg_core::pg_client::pool_acquire::pool_get_connection_inner_excluding
#3  plex_pg_core::pg_client::pool_lookup::pool_find_connection_for_db
#4  rust_pg_find_connection
#5  sqlite3_prepare_v2 / rust_my_sqlite3_step
```

**The cause.** The zombie reclaim asked whether a slot's owner thread was
still alive with `pthread_kill(owner, 0)`. Plex runs on its own musl, where a
`pthread_t` is a pointer to the thread's control block and `pthread_kill`
dereferences it to take the kill lock. Once the owner has exited -- and Plex's
request, webhook and pool threads are short-lived -- that block is unmapped,
and the thread acquiring a connection faults. The 300-second idle guard in
front of the probe is why nothing could die in the first five minutes, and a
playback burst is what runs the reclaim pass on many threads at once, which is
why "it dies when I press play". `.14` had already made the reference count
the decision and kept the probe as a hint; the hint was the crash. It is
deleted, not disabled.

**Reproducer**, which killed Plex within a second before the fix and is the
check after it: eight concurrent playback-start sequences (metadata with
`includeBandwidths`, the thumbnail, timeline `buffering`/`playing` reports)
with the owner token from `Preferences.xml` -- the local admin token gets 401
on `/:/timeline` -- against one pod, more than five minutes after its Plex
started. One client, sequentially, does not reproduce it.
Run after `.19` rolled out, past the five-minute window, against all three
pods in turn -- eight clients, three rounds each -- and then three concurrent
HLS transcode sessions per pod: the kernel's fatal-signal count did not move,
no request failed and every Plex stayed on its first start.

Two things seen along the way and not chased: the shim's `STACK_CHECK` lines
show it running on Plex threads with 124--128 KB stacks and ~110 KB of
headroom, and the pool auto-grew 59 to 69 slots in one minute under the burst.

## Fixed: a warm pool never shrank

After the crash fix the pods held 35--48 PostgreSQL connections each, all
idle, some for a quarter of an hour, and the count only ever went up. Fixed
in `v1.3.17-clusterplex.20`.

**The cause.** The pool's maintenance -- reclaiming a slot whose owner thread
has moved on, and closing connections idle past `PLEX_PG_IDLE_TIMEOUT` -- ran
in exactly one place: after a thread missed the phase-1 fast path of an
acquire. On a warm pool every Plex thread already owns a READY slot, so phase
1 answers every acquire and nothing misses. Every "Pool reaper: running" line
in every pod's log sits inside startup or a playback burst, when new threads
were arriving; there is none after the last burst, however long the pod ran.
Connections were closed only when the *next* burst brought new threads, and
by then it had opened more.

**The fix.** The pass also runs at the top of the acquire whenever the
reaper's interval (60 s) has elapsed since it last ran, whatever the acquire
does next. When it is not due the fast path pays one atomic load. A thread
that needs a slot still reclaims immediately. So an unreferenced connection
now lives at most the idle timeout plus one reaper interval after its last
use, as long as anything at all queries the database -- and the manager's
library probe does, every few seconds. Handles Plex keeps open (about twenty
at startup) pin their slots and are not touched.

Verified on the kind cluster: a playback burst took the pods to 33, 50 and
34 connections; nine minutes later, with only the manager's probe querying,
they were at 9, 23 and 12, with a reaper run on every pod every minute.

**Test.** The shim's unit test drives the real acquire on a pool it owns, with
the calling thread on the fast path (libpq's `PQstatus` stood in for by a
`cfg(test)` thread-local, so no server is needed) and another thread's
connection idle and unreferenced past the timeout. Before the change the
acquire returned the caller's connection and left the other open.

Also in `.20`: `Pool: auto-grew N -> M` is logged at INFO. It was ERROR, and
fifteen of them per burst read as fifteen faults; growth is the pool doing
its job.

## Fixed: a movie played again once it finished

Every play queue built from one movie held the movie twice, at orders 1000
and 2000, so when the first playback finished the client moved on to the
"next" item and played the same movie again. Five of the six play queues in
the database had it, one of them created by a plain `POST /playQueues` with
no client involved. Fixed in `v1.3.17-clusterplex.21`.

**How it was found.** The postgres pod logs every statement
(`log_statement = all`), so a controlled `POST /playQueues` for the movie
gave the exact SQL. Plex itself issued two `INSERT INTO play_queue_items`,
two milliseconds apart -- its item query had returned the one movie twice --
and its answer to the request listed the second item twice as well. Around
those inserts, every SELECT that Plex steps more than once had run twice: the
same prepared statement, on two different connections, four milliseconds
apart. `SELECT ... LIMIT 1` queries, which the shim fetches eagerly, ran once.

**The cause.** The shim resolved a statement's connection through the pool on
every `sqlite3_step`. A streaming SELECT marks its connection
`streaming_active` when it returns its first row, and the pool refuses to
hand a thread the slot it is streaming from, so the statement's own second
step was given a different connection. `should_clear_cross_thread_result`
took a result connection that differs from the execution connection as the
statement having crossed threads, cancelled the stream and ran the query
again eagerly -- from row one. So the first row of every streamed result was
delivered twice. Listings survive it because Plex keys them by id; play
queues append every row they are given.

**The fix.** A statement that is mid-stream stays on the connection it is
streaming from when the stepping thread is the one that started it; the pool
is only asked when it is not. Another thread stepping the statement still
takes the requery path, which is what the check was written for -- and that
path still replays already-delivered rows, so it remains a hazard if Plex
ever does step a streaming statement from another thread. Nothing observed
says it does.

Verified on the kind cluster: the same `POST /playQueues` now yields one
item, one insert, and every read in the request executed on one connection.

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
