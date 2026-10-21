# Live TV guide loads: copied at PMS's own rename

Status: proposed, 2026-10-21; the owner chose this design over the first
draft the same day. It replaces item 1 of the "As built" section of
`2026-10-20-epg-in-postgresql-design.md` (the manager copies each load on a
30 s poll) with a copy that runs at the moment PMS swaps its load in.
Everything else in that design stands.

## Problem

PMS builds every guide load in `<guide>-loading.db` and renames it onto the
guide's file. The shim passes that file through to SQLite, and the manager
copies each finished load into the guide's schema (`ImportGuide`, polled
every 30 s by `copyGuideLoads`). That costs:

- **A stale window.** After the rename, PMS reopens the guide, which the shim
  serves from PostgreSQL. The schema still holds the previous load for up to
  30 s plus the copy (3.2-3.3 s for 12,8xx rows on talos, 2026-10-21 16:25
  and 16:28Z). Then every row is renumbered under PMS: a load numbers its
  rows from 1. Whatever PMS does just after a swap reads the old guide.
- **Failures PMS cannot see.** A failed copy leaves PostgreSQL stale while
  PMS believes it swapped.
- **Mixed reads.** Until the next start the pod's guide file holds the real
  load, not a shadow, so the reads the shim lets reach SQLite disagree with
  the ones it answers from PostgreSQL.
- **No statistics** until autovacuum reached the new tables. Fixed on its
  own in cluster-plex `a12d272`: `ImportGuide` analyzes before it swaps.

## What was found (PMS 1.43.4.10903, the rig image; talos runs 1.43.4)

- **The shim already sees the load.** PMS opens `-loading.db` through
  `sqlite3_open_v2`, which the shim interposes. The `.26` fix was about
  statements on exactly that handle (`hack/plex-postgresql/README.md`, "How
  PMS swaps a guide", 2026-10-07). It is passed through only because
  `epg_schema_for_path` wants the name to end in a uuid.
- **The swap is a libc call the shim can hook.** The template copy is
  `boost::filesystem::copy_file` (`sendfile`) and the swap is
  `boost::filesystem::rename`. `libboost_filesystem.so` imports musl's
  `rename` dynamically; `Plex Media Server` itself imports neither `rename`
  nor `renameat`. The shim already overrides libc's `sigaction` this way
  (`rust/plex-pg-core/src/runtime_linux.rs:282`).
- **The template is from 2018.** `Resources/com.plexapp.plugins.library.db`
  holds 122 migrations (to `20181210190600`) and 71 tables. PMS migrates
  each load to the 442 the library holds, in SQLite, in-process.
- **The shim forwards PMS's transactions** (`pg_client/txn.rs`, since
  2026-10-16), and makes no savepoint per statement, so a statement
  PostgreSQL refuses inside one of PMS's transactions aborts the whole
  transaction, where SQLite carries on. A commit's WAL flush on talos
  costs about 0.43 ms (2026-10-21: 1,000 single-message transactions,
  601-634 ms with `synchronous_commit = on`, 172 ms off).
- **The copy is cheap and tested.** `ImportGuide` is a binary `COPY` of
  every shared table in one transaction with user triggers held off, then
  `ANALYZE`, then a schema rename (which locks none of the schema's tables,
  so it never queues behind a reader). Of its 3.3 s, the clone of the
  library schema into staging is most; the copy and swap are about 0.5 s.
- **The manager is PMS's parent.** `Supervisor` starts PMS with
  `exec.Command` and the environment `shimenv.go` builds, so the shim can
  be handed a socket path the way it is handed `PLEX_PG_SCHEMA`.

## Why not load through the shim

The first draft of this design had the shim redirect `-loading.db` to a
staging schema, so PMS wrote the load into PostgreSQL itself. It was not
taken because:

- every INSERT and every lookup PMS makes while loading would become a
  translated round trip (a bare `SELECT 1` costs 0.18 ms over the
  database's socket; SQLite does it in-process), against a database the
  library also uses;
- one translation gap inside a PMS transaction would lose the whole batch;
- it needed a load registry deciding each connection's schema on the shim's
  hottest path, a replaced template so PMS skips its migrations, and
  reconnect checks, none of which this design needs.

A slower load is invisible to viewers either way (the previous guide stays
served until the swap), so the gain was only "no guide bytes in a file",
and the cost was the shim's riskiest change yet.

## Design

PMS loads into SQLite at its own speed, as today. The copy runs when PMS
renames the load onto the guide, not on a poll, and the rename waits for
it.

### The shim: `rename()`

`rename(old, new)` is exported like `sigaction`. It passes straight through
unless `old` names `tv.plex.providers.epg.<provider>-<uuid>-loading.db` and
`new` the guide's file (`epg_load_for_path`, with the `-wal`, `-shm` and
`-journal` suffixes recognised and ignored), and `PLEX_PG_GUIDE_SOCKET` is
set. Then it:

1. connects to the Unix socket `PLEX_PG_GUIDE_SOCKET` names and sends one
   line, `swap <old path>\n`;
2. waits for one line back: `ok\n`, `skip <reason>\n` or `fail <reason>\n`,
   with a 60 s deadline (a copy takes 0.5 s; the deadline is for a wedged
   manager);
3. logs the answer, and calls the real `rename` whatever it was.

A refused connection, a timeout or a malformed answer is logged at ERROR and
the rename passes through: the poll (kept, below) copies the load within
30 s, as today. The hook never fails PMS's rename. Failing it (`EIO`) would
put PMS on its own "caught an exception trying to swap out the database"
path, whose behaviour nothing has recorded; the rig run below records it,
and the choice is revisited then.

The hook runs in every process the shim loads into, the Transcoder and
Scanner included. With no `PLEX_PG_GUIDE_SOCKET`, or a path that is not a
load, it costs one string comparison.

### The manager: the guide socket

The manager listens on `<PlexDir>/Plug-in Support/Databases/clusterplex-guides.sock`
(a path no shim redirect, `copyGuides` glob or PMS file matches), created
before PMS starts, mode 0600, and passes it as `PLEX_PG_GUIDE_SOCKET`.

On `swap <path>`:

1. `GuideForFile` on the path with `-loading` removed gives the guide; a
   path that is not one, or not under the pod's Databases directory,
   answers `fail not a guide load`.
2. Take `guides.mu`, as `copyGuides` does, so a poll and a swap never copy
   the same file at once.
3. Run `ImportGuide` on the load file. It copies only a file holding airings
   that is newer than the schema's comment, so an empty or older load
   answers `skip <reason>` and the previous guide stays. The load file is
   closed by PMS at this point (PMS closes it before the rename), so the
   read sees every page.
4. Answer `ok` (copied), `skip`, or `fail <error>`.
5. Record the file's state in `guides.seen` so the poll does not copy it
   again under its new name.

Each request is served on its own goroutine; the lock serialises the copies.

### Pre-cloning the staging schema

The clone of the library schema is most of the copy's 3.3 s. To keep PMS's
rename wait near 0.5 s:

- the manager watches the Databases directory with fsnotify (already in the
  module, indirect) for the creation of a `-loading.db`; polling the
  directory every 5 s during a `provider.epg.load` activity is the fallback
  if the watch misbehaves on the emptyDir;
- on a new load it clones `epg_<uuid>_next` at once (`CloneSchema`, under
  the guide's advisory lock) and remembers it;
- `ImportGuide` takes an existing, empty `_next` instead of dropping and
  recloning it. It is empty by construction: nothing but `ImportGuide`
  fills it, and `ImportGuide` drops it on any failure.

A load PMS abandons leaves a `_next`; the next `ImportGuide` of that guide
truncates it before copying, so nothing stale is ever swapped in, and the
sweep below drops one with no load file beside it.

### A new DVR

The DVR's first load renames onto a guide with no schema. `ImportGuide`
makes one (it already does, "a guide with no schema gets one, with the load
in it"), the rename passes, PMS reopens the guide, and the shim finds the
schema: the guide is in PostgreSQL from its first load, with no restart. No
pre-clone exists for it, so this one swap waits the full 3.3 s.

### Kept, and swept

- **`copyGuideLoads` stays**, as the fallback for a rename the hook did not
  see (no socket, a timeout, a future PMS renaming by a call the shim does
  not hook). After a hooked swap the poll finds the file's state recorded
  and copies nothing. Its 30 s stale window is then the exception, logged
  when it happens, not the rule.
- **`PrepareGuides` sweeps**, under each guide's advisory lock:
  - `_next` schemas with no load file beside them, and `_old` ones, under
    the 5 s lock timeout;
  - live schemas of DVRs the library no longer lists (DVR 2's
    `epg_adfe4a6f…` on talos today). A DVR's row is written when it is
    created, before its first load, so a new DVR is never swept.

### Considered and not taken

- **Loading through the shim.** Above.
- **The shim running the copy itself.** It would need `COPY` streaming and
  the schema clone in Rust, duplicating `ImportGuide`, in a library loaded
  into PMS. The manager already has the tested copy and the connection.
- **A watch on the rename instead of a hook** (fsnotify on the guide file).
  It would still leave a window between PMS's rename and the copy, and the
  file would be reopened by PMS, which the shim then serves from the stale
  schema. The hook is the only point where the swap lands before the
  reopen.
- **An HTTP endpoint instead of a socket.** The shim links no HTTP client;
  one line over a Unix socket is the smallest thing that works, and the
  socket's mode scopes it to the pod's user.

## Testing

- **Shim unit tests:**
  - `epg_load_for_path`, with and without the suffixes;
  - `rename` passes through for unrelated paths, without the variable, and
    when the socket refuses, times out or answers nonsense;
  - the request and answer framing, against a test server on a socket.
- **cluster-plex tests:**
  - the socket server: a `swap` of a loaded file copies it (airings in the
    schema, `ok`), an empty load answers `skip`, a path outside the directory
    answers `fail`, and a concurrent poll does not copy the same file twice;
  - `ImportGuide` reuses a pre-cloned empty `_next`, and truncates one left
    by an abandoned load before copying;
  - the pre-clone fires on a `-loading.db` creation;
  - the sweep drops an orphan `_next`, `_old`, and an unlisted DVR's
    schema, and keeps the listed ones.
- **The standalone rig**, before talos:
  1. A guide load ends with the swap in the hook: the manager logs the copy
     before PMS's "swap out the database" line, and the grid answers from
     the new rows at PMS's reopen.
  2. The rename's wait, with and without a pre-clone.
  3. A new DVR's first load lands in PostgreSQL with no restart.
  4. With the socket unset, the poll copies the load as before.
  5. PMS killed mid-load: the next start's sweep drops the `_next`.
  6. What PMS does when `rename` fails with `EIO`, to settle whether a
     failed copy should refuse the rename.
  7. The usual concurrent pass during a guide load: browsing, search, and
     100+ `/library/sections` probes answering 200.

## Rollout

1. `ANALYZE` in `ImportGuide`: cluster-plex `a12d272`, done.
2. cluster-plex: the socket server, the pre-clone, the `seen` record after
   a hooked swap, the sweep. Works with any shim: without the hook, the
   poll copies as today.
3. The shim: `rename` hook and `epg_load_for_path`, released as the next
   `v1.3.17-clusterplex.N`; the cluster-plex image bumps `PLEX_PG_REF` and
   sets `PLEX_PG_GUIDE_SOCKET`.
4. The rig runs above, then talos. No PMS restart is needed beyond the image
   rollout itself.

## Risks

- **The rename hook blocks PMS's load thread** for the copy (0.5 s, or
  3.3 s for a first load). PMS holds no request lock during the swap that
  anyone has seen; the rig's concurrent pass is the check.
- **A wedged manager holds the rename for 60 s** and then the load lands in
  SQLite, copied by the poll once the manager answers again. PMS's own
  behaviour under a slow rename is unrecorded; the rig records it.
- **The socket is reachable by any process in the pod's user**, which is
  PMS's. A request only ever copies a load file PMS wrote, under the
  directory check, so the worst a stray request does is an extra copy.
