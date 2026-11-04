# Live TV guide loads in PostgreSQL

Status: proposed, 2026-11-04. Not approved to build. It replaces the copy
half of `2026-10-21-guide-loads-copied-at-rename-design.md`: PMS's load of a
guide goes into PostgreSQL as PMS makes it, and the rename hook swaps a
schema instead of copying a file. The schema-per-guide design
(`2026-10-20-epg-in-postgresql-design.md`) and the hook's framing stand. The
manager-written loader (`2026-11-04-cloud-guide-loader-design.md`) stays
parked: it attacks the load's length, which this design does not touch.

## Problem

A guide load is the one place a guide still lives in SQLite. PMS copies its
bundled template to `<guide>-loading.db`, fills it in-process, closes it and
renames it onto the guide; the shim's `rename()` hook has the manager copy
the file into `epg_<uuid>` first (35 s for a 354,116-row cloud guide, half a
second for XMLTV), and the guide is served from PostgreSQL from PMS's reopen.
What that leaves:

- a 35 s stall in PMS's load thread at every cloud-guide rename, which grows
  with the lineup;
- a second copy of every load on the pod's emptyDir, read whole by the
  manager, with a copy step that can fail after PMS believes it swapped;
- a genuine SQLite handle inside PMS with PMS's own tokenizer on it, which
  the shim must leave alone statement by statement (shim .44's
  `handle_is_pg_backed` gate exists only for it).

The shim already sees the load: PMS opens `-loading.db` through the
interposed `sqlite3_open_v2`. It is passed through because
`epg_schema_for_path` wants the file's stem to end in the DVR uuid, and
`-loading` breaks that match. Everything a PostgreSQL-backed guide handle
needs exists: a schema per guide with its own connection and search path,
the FTS views, VACUUM as a no-op, DDL forced to PostgreSQL, no statement
timeout, and the hook at the rename.

## What was measured (talos, PMS 1.43.5, 2026-11-04)

- **PMS migrates every load.** On the fresh template it runs 145 forward
  migrations (`20190130000000` on) and four VACUUMs, 3.5 s per load
  (13:28:25.9 to 13:28:29.4 in the debug log), then loads. Against a schema
  cloned from the library all 443 migrations are recorded, so PMS runs none,
  as it runs none on the library.
- **The load's time is PMS's own.** `Step 2/2 (network: 512.6 sec,
  database: 1651.1 sec)` for 248 channels: the database phase is one thread
  at 86% of a core parsing and inserting. A load writes about 480,000 rows
  (85,620 `media_items`, 85,620 `media_parts`, 72,044 `metadata_items`,
  236,346 `taggings`, 1,168 `tags`) in bulk statements: the cloud loader's
  INSERT carries more than 225 placeholders, which is what hit the C shim's
  4 KiB dummy-shadow buffer. A round trip over the database's socket is
  0.18 ms, a commit flush 0.43 ms, so even 500,000 statements add about two
  minutes to a 27 to 37 minute load. The October objection to loading
  through the shim ("every INSERT a round trip") was written before these
  numbers.
- **The one loader statement we know** is the dupe lookup `select
  metadata_items.id, media_items.id from media_items join metadata_items
  ... join fts4_metadata_titles_icu on ... where
  fts4_metadata_titles_icu.title match ? and metadata_items.metadata_type
  = 4`, which found "a dupe show 'Catholic Mass' with a different GUID".
- **A failed load re-pairs the lineup.** When every cloud load failed on
  the FTS block (Error 25), PMS replaced the DVR's channel map with its own
  pairing, and a reload sent during a load deleted a DVR. The converger
  saves a map only when the lineup hash moves (`converge.go`,
  `s.lineup != b.LineupHash`), so a map PMS replaced stays replaced.
- **PMS opens a guide on about ten handles** (the fd counts of the WAL-leak
  incident: ten per generation). Each guide handle is its own PostgreSQL
  connection, never a pool slot.
- **The shim's read cache is per thread, keyed by SQL and parameters, 1 s
  TTL, at most 5 rows, and a write invalidates only its own key.** A lookup
  that answered no rows, then an insert, then the same lookup within a
  second is answered from the cache. The loader's dupe check is exactly
  that pattern.
- **A failed statement poisons a PostgreSQL transaction.** The shim forwards
  PMS's transactions on the handle's connection and makes no savepoint per
  statement, so where SQLite carries on after a refused statement,
  PostgreSQL refuses everything until ROLLBACK.

## Design

PMS loads as it does today, into what it believes is `-loading.db`. The
shim routes that handle to the guide's staging schema `epg_<uuid>_next`,
and the file PMS opens is a shadow the manager put there. At PMS's rename
the manager renames the schemas and PMS's rename moves the shadow onto the
guide. Nothing of the guide is ever in SQLite.

### Names

- The load's schema is `epg_<uuid>_next`, cluster-plex's `Guide.Staging()`,
  the same name the copy design stages into. The shim's
  `epg_schema_for_path` (`pure_helpers.rs`) accepts a stem ending in
  `-<uuid>-loading` and answers `_next` for it; `is_guide_path`,
  `should_redirect_str` and `guide_swap_paths` follow, as they take their
  answer from it.
- The guide socket gains one verb, `stage <load file>`, beside `swap`.
- `PLEX_PG_GUIDE_LOADS` on the shim: unset or `0` leaves every load in
  SQLite (today); `1` routes a load the manager stages. cluster-plex sets it
  from `plex.liveTV.guideLoadsInPostgres` (chart, default `false` until the
  rig runs pass, then `true`). `SHIM_PASSTHROUGH_ONLY` overrides both.

### The staging schema's states

`_next` carries its state in its schema comment, the way the live schema
carries the copied file's mtime today:

| comment | meaning |
| --- | --- |
| (none, or `clusterplex: staging, clean`) | cloned from the library, nothing written |
| `clusterplex: staging, loading since <Unix ns>` | PMS is loading into it |
| swapped away | it became `epg_<uuid>`, commented `clusterplex: loaded by PMS at <Unix ns>` |

The clone is `CloneSchema` as today (3.3 s), with the sequences at 1 so a
load numbers its rows from 1 as SQLite does.

### The manager at start (`prepareGuides`)

For every guide the library lists: drop `_next` whatever its state (PMS is
the manager's child, so a start means no load is running), remove any
`-loading.db` and its `-wal`/`-shm` from the Databases directory, keep
`ImportGuide` for a load file the pod still holds from an old shim (the
copy path, unchanged), and build the guide's shadow as today. Then clone a
clean `_next` per guide in the background once PMS is up, so the first load
of each guide finds one. The `_next` clause of `SweepGuides` goes: the
start rule replaces it.

The manager also builds **one shadow template** at start,
`Databases/.clusterplex/guide-shadow.db`, through `BuildShadow` (the
library's SQLite schema plus every `schema_migrations` row), with
`PRAGMA application_id = 0x434C5058` (`CLPX`) as the mark of a file the
manager wrote. The library's and the guides' shadows get the same mark.

### The shim at the open of a load

In `rust_my_sqlite3_open_v2`, before the real open, for a path
`epg_schema_for_path` reads as a load and with `PLEX_PG_GUIDE_LOADS=1`:

1. send `stage <path>\n` on `PLEX_PG_GUIDE_SOCKET` and wait for one line,
   up to 60 s (the manager's own deadline on the request is 45 s, so a slow
   clone ends in the manager's reason, never the shim's timeout, and the
   manager never touches the file after the shim stopped waiting);
2. on `ok`: open the path (now the shadow) and connect the handle to
   `_next` as `rust_pg_connect` connects any guide, through its existing
   `schema_exists` check; log `GUIDE_LOAD: staged <path> into <schema>`;
3. on `skip`, `fail`, no socket, a refused connection or a timeout: open
   the path as it is and register no connection, which is today's genuine
   SQLite load, copied by the hook at the rename; log at ERROR why.

PMS opens the load on about ten handles. Every open asks; the manager
answers a second open of a staged load in under a millisecond (below), so
the shim keeps no memo and cannot hold a stale one across loads.

### The manager on `stage <path>`

Under `guides.mu`, with the Databases-directory and `GuideForLoadFile`
checks `swap` already makes:

1. Read the file. PMS's template copy carries no mark; the manager's
   shadow carries `CLPX`.
2. Read `_next`'s state.
   - File unmarked, `_next` clean or absent: a new load. Clone `_next` if
     absent (the fsnotify pre-clone usually got there first), mark it
     `loading since now`, write the shadow (below), answer `ok`.
   - File unmarked, `_next` loading: an abandoned load (PMS was killed, or
     a load failed and PMS copied a fresh template). Drop `_next`, clone,
     mark, write the shadow, answer `ok`. The drop costs the clone's 3.3 s
     once.
   - File marked, `_next` loading: another handle of the running load.
     Answer `ok` and touch nothing.
   - File marked, `_next` clean or absent: a shadow left by a load that
     never wrote (the manager restarted, which restarts PMS, so this is
     the start rule's leftover). Treat as a new load.
   - Any error, or the deadline: answer `fail <why>` without touching the
     file, so PMS loads into SQLite as today.
3. Writing the shadow: copy `.clusterplex/guide-shadow.db` to
   `<path>.clusterplex-tmp` and `rename(2)` it over `<path>`, after
   removing any `<path>-wal`/`-shm`. PMS's `copy_file` has closed its copy
   before the open, so nothing has the template open. The manager's own
   rename runs in Go, outside PMS, so the shim's hook does not see it.

An `ok` answer promises both: `_next` exists and the file is a shadow.
`rust_pg_connect`'s `schema_exists` check stays as the net under it; a
handle that fails it opens a shadow as genuine SQLite, whose load fails at
the first FTS lookup (the shadow has no FTS module). That is the outcome
of PostgreSQL vanishing between the answer and the connect, and the
library would be failing at the same moment.

### What a load handle does differently

A statement on a handle whose path `epg_schema_for_path` reads as a load:

- **No result cache.** The cached-read path answers nothing for it, and
  stores nothing (`db_interpose_step_cached_read_utils`). The dupe check
  must see the insert before it.
- **A savepoint per write statement inside a transaction**
  (`db_interpose_step_write_utils`): `SAVEPOINT plex_pg_stmt` before the
  statement, `RELEASE` after it, and on an error `ROLLBACK TO` then
  `RELEASE`, so the transaction stays usable as SQLite's does and PMS reads
  the statement's own error through `sqlite3_errcode`. Two round trips per
  statement, about 0.4 ms; `PLEX_PG_GUIDE_LOAD_SAVEPOINTS=0` turns it off
  once the rig shows the loader never continues past an error.
- **Everything else as a guide handle:** its own connection on
  `search_path epg_<uuid>_next, public`, no statement timeout, FTS
  rewritten to the views, VACUUM and REINDEX as no-ops, DDL to PostgreSQL
  (none expected: the migrations are recorded), `last_insert_rowid` from
  the connection the last write ran on, the dummy shadow carrying every
  placeholder (.44), transactions pinned to the handle.

The shadow matters only for what the shim lets reach SQLite: `PRAGMA`s
(`journal_mode=WAL` makes a `-wal` and `-shm` beside it, gone at the last
close), `sqlite_master`, `sqlite_stat*`, `fts3_tokenizer` and
`icu_load_collation`. Column types never come from it: the shim carries
them generated from `sqlite_column_types.sql`, and the decltype preload
reads the shadow's full 442-migration schema.

### The hook at the rename (`swap <path>`)

The shim's `rename()` hook is unchanged in protocol and framing. The
manager's `swap` handler reads the file first:

- **Marked (a PostgreSQL load):** under the guide's advisory lock,
  `ANALYZE` `_next` (one to three seconds for a cloud guide), `swapGuide`
  (`epg_<uuid>` to `_old`, `_next` to `epg_<uuid>`, in one transaction
  under the 5 s lock timeout), comment the live schema `loaded by PMS at
  <now>`, drop `_old` as today, answer `ok`, and clone the next clean
  `_next` after answering. Record the guide file in `guides.hooked` so the
  poll's `ImportGuide` leaves it alone (it would anyway: a shadow holds no
  airings). The rename wait drops from 35 s to the ANALYZE.
- **Unmarked (a SQLite load, the fallback):** today's `ImportGuide` copy.

Then PMS's rename moves the shadow onto the guide, PMS reopens the guide,
the shim finds `epg_<uuid>` and serves the new load. The old handles close
through the real finalize and close (.41). The guide's on-disk file is a
proper 442-migration shadow from then on, which is better than today's,
where the renamed file is the whole SQLite load until the next start
rebuilds it.

### Recovery

- **A rename the manager did not answer** (deadline, a socket error): the
  rename passes, PMS serves the old guide from `epg_<uuid>`, and `_next`
  holds a complete load marked `loading`. The `stage` records the load
  file's inode in the mark, and a rename carries the inode with the file,
  so on its next pass the poll swaps a `_next` marked `loading` whose load
  file is gone from the directory and whose guide file now has that inode.
  (The first cut keyed this on the DVR's `refreshedAt`, the provider row's
  `updated_at`; the rig showed a failed from-scratch load leaves it alone,
  and talos that a cancelled incremental refresh moves it, 2026-11-04 --
  so a failed load's leftover `_next` would have been swapped in at the
  next refresh.)
- **A failed load** (PMS logs `Failed to load EPG`, removes the file, and
  re-pairs the channels): the poll finds `_next` marked `loading`, no load
  file and the guide file's own inode, drops the schema and clones it clean
  again; and the converger restores the map (next).
- **The map PMS replaced.** After any load ends on a cloud block (the
  DVR's `refreshedAt` moved, or a `stage` was answered and no `swap`
  followed within the load's usual time), the converger reads the DVR's
  channel map from PMS and compares it with the tuner's pairing; on a
  difference it saves the map again, Event `DVRChannelMapRestored`. A save
  starts a whole load, so only a real difference saves. This guard is worth
  having whatever becomes of the rest, since the Error 25 week showed the
  failure without it.

### Several Plex pods

`replicas` is 1 on talos. With more, each pod's manager and PMS would load
into a staging schema of their own, `epg_<uuid>_next_<ordinal>`, swap into
the shared `epg_<uuid>` under the advisory lock, and the `loaded by PMS at`
comment would order the swaps so an older load never replaces a newer one,
as the file mtime orders the copies today. Not built now; the names leave
room for it.

## Costs and risks

- **Fidelity of the dupe check.** PMS asks the ICU collating tokenizer at
  primary strength; PostgreSQL answers `to_tsvector('simple',
  plex.fold(title)) @@ plex.fts_query(?)`, unaccent-and-lower split on
  punctuation. A title the two tokenize differently changes which shows
  merge. The oracle below is the only judge; a difference it finds is a
  translator change (the FTS views' expression), not a reason to copy
  files.
- **An unknown statement set.** The loader's SQL has never gone through
  the translator. One statement it refuses fails the load, and a failed
  load costs a 30-minute reload and, without the map guard, a re-paired
  lineup. The kill switch stays off on talos until the rig has run the
  XMLTV and the cloud load end to end. The pattern of the misses so far
  (`.47`, `.48`, `.49`, every one found on PMS's incremental refresh,
  which runs the loader's lookups against the live guide) is stable in
  one way: each is a collation case -- two in the collation preprocessing
  (`preprocess.rs`'s `rewrite_sqlite_collations`: an IN list's collation,
  a doubled quote in a literal) and one in the AST quotes pass
  (`quotes.rs`'s `fix_expr` had no arm for `Expr::Collate`, so a
  backtick-quoted identifier under a kept collation never became
  double-quoted) -- and each showed only on data that reached that
  statement: an IN list, a title with an apostrophe, a show whose
  episodes PMS ordered by `parents.\`title_sort\``. A load from scratch sends the same lookups
  (its step 2 asks `/library/metadata/<id>` per programme), so the rig
  runs on `.49` or later, and the XMLTV guide's titles are chosen to
  reach the ORDER BY paths (below).
- **Savepoints double the round trips** of every write in a transaction:
  about 0.4 ms each, on a load whose own work is 1,651 s. Measured on the
  rig with the switch on and off.
- **Ten more PostgreSQL connections** for the load's length, outside the
  pool, on clustarr-plex-db-1, beside the library pool
  (`PLEX_PG_POOL_SIZE` to `PLEX_PG_POOL_MAX`); check `max_connections`
  leaves room.
- **Write load on the shared database** for the load's length: about
  500,000 rows and their index maintenance (the clone brings the GIN and
  trigram indexes with `INCLUDING ALL`), spread over 30 minutes. Nothing
  the library's own scans do not do.
- **The open blocks PMS's load thread** for the stage: milliseconds with
  a pre-cloned `_next`, 3.3 s without, 45 s at worst before the manager
  answers `fail` and the load falls back to SQLite.
- **What it does not buy:** a shorter load. The 30 minutes are PMS's
  serial metadata fetches and parsing. Only the manager-written loader
  changes that.

## Testing

### Shim unit tests

- `epg_schema_for_path` on a load file, with the `-wal`/`-shm`/`-journal`
  suffixes, and on a load whose uuid is not one; `guide_swap_paths`
  unchanged.
- The open: `stage` answered `ok` connects to `_next`; `skip`, `fail`, no
  socket, a timeout and `PLEX_PG_GUIDE_LOADS` unset each leave the handle
  genuine SQLite, and log why. The `rename` tests' socket server gains the
  verb.
- A load handle's read bypasses the cache; a library handle's still hits
  it.
- A write inside a transaction on a load handle is wrapped in a savepoint;
  a refused statement leaves the transaction open and the next statement
  runs; the switch turns the wrapping off.

### cluster-plex tests

- `stage`: a new load clones and marks `_next` and writes a marked shadow
  over the template; a second request on the staged load touches nothing;
  an abandoned load's `_next` is dropped and recloned; a request after the
  manager's deadline writes no file; the path checks refuse a file outside
  the directory.
- `swap`: a marked file swaps the schemas and answers `ok` without a copy;
  an unmarked file still copies; the next clean `_next` is cloned after.
- The start rule: stale `_next` schemas and load files are removed, the
  shadow template is built with the mark, the per-guide `_next` is cloned
  in the background.
- Recovery: a `loading` `_next` with no file is swapped by the poll when
  the guide file carries the load file's inode; with another inode it is
  dropped and cloned clean; a mark without an inode is left for the next
  `stage`.
- The map guard: a map PMS replaced is saved again once, and an unchanged
  map is not.
- `BuildShadow` sets the mark; `readShadowMark` reads it.

### The oracle: load the same guide both ways and diff the schemas

A tool `hack/guidediff` (or a test helper) compares two guide schemas table
by table: row counts; `metadata_items` keyed by `guid` (`metadata_type`,
`title`, `title_sort`, `index`, `year`, `duration`, `content_rating`,
`tags_genre`, `originally_available_at`, the parent's guid, a hash of
`summary`); `media_items` keyed by `channel_id` and `begins_at` (`ends_at`,
the item's guid, `extra_data`); `media_parts` per item; `tags` by
`tag_type` and `tag`; `taggings` by the item's guid, the tag and `index`.
Ids and the load's own timestamps are ignored.

- **Deterministic:** an XMLTV DVR on a fixed guide file (the rig's stub)
  loaded with the switch off (copied at the rename) and with it on (loaded
  into PostgreSQL) must diff empty. This is the merge-fidelity test.
- **Live:** the DIRECTV Mobile lineup loaded both ways minutes apart; the
  only differences allowed are the service's own changes at the horizon.

### The standalone rig (`docker-compose.standalone.yml`), before talos

1. An XMLTV load with the switch on: PMS's log shows no migration on the
   load, the stage answers in milliseconds with a pre-cloned `_next` and
   in about 3 s without, the hook's wait is the ANALYZE, PMS's grid answers
   from `epg_<uuid>` at the reopen, the guide's file is a marked shadow,
   and the oracle diff is empty.
2. The cloud lineup, with the switch on: the load completes, the diff is
   within the horizon, and the load's length with savepoints on and off.
3. PMS's guide fds across two swaps: no `(deleted)` handles, no growth.
4. The switch off, and the socket absent: the copy path works as before.
5. PMS killed mid-load, then restarted: the start rule clears `_next` and
   the file, the next load stages fresh.
6. A refused statement injected on a load handle (a test hook in the
   shim): with savepoints the load continues; without, it fails, PMS
   re-pairs, and the converger restores the map.
7. The hook answered late (a sleep in the manager's `swap`): the rename
   passes, the poll swaps within 30 s, PMS serves the new guide after it.
8. The usual concurrent pass during a load: browsing, search, 100+
   `/library/sections` probes answering 200, and a Live TV tune on the
   previous guide.

## Rollout

1. **cluster-plex first**, working with the current shim: the mark in
   `BuildShadow` and the shadow template, the start rule, the `stage` verb,
   the `swap` branch for a marked file, the recovery rules, the map guard,
   the chart value and the `PLEX_PG_GUIDE_LOADS` env, the oracle tool and
   the tests. With `.46` the verb is never asked and the value does
   nothing, so this ships on its own.
2. **plex-postgresql** `v1.3.17-clusterplex.47`: the path rule, the
   open-time stage, the load handle's cache bypass and savepoints, the unit
   tests; cluster-plex's Dockerfile bumps `PLEX_PG_REF`.
3. **The rig**, the eight runs above, with the oracle diff recorded in this
   document's "As built" section.
4. **talos**, value off, through cluster-bootstrap as every deploy; then
   the value on, watching one load end to end (a channel-map change or the
   daily reload, never a hand-sent `reloadGuide`): the manager's `stage`
   and `swap` lines, PMS's rename wait, the grid at the reopen, and the
   diff of the new `epg_<uuid>` against `_old` for the overlapping window
   before `_old` is dropped.

Nothing restarts beyond the image rollouts. The first talos load after the
switch is a dress rehearsal of the fallback too: a `fail` from `stage` is
today's load.

## Considered and not taken

- **The shim substituting the opened path** (open a prepared shadow beside
  PMS's template copy, and rename it into place at the hook instead of
  PMS's file). It keeps PMS's own rename from ever landing, so the hook
  would have to answer PMS's `rename` itself, remove PMS's copy, and own
  every failure path PMS has around that file. Writing the shadow over
  PMS's copy before the open leaves every file expectation PMS has intact.
- **The shim cloning the schema itself.** `CloneSchema` is tested Go in the
  manager, which already answers the socket; a second copy in a library
  loaded into PMS buys nothing.
- **Disabling the result cache globally** (`PLEX_PG_DISABLE_QUERY_CACHE`).
  The library's hot paths lean on it; the load handle is the one place the
  read-after-write pattern is proven, so the bypass is per handle.
- **Dropping the load's indexes and rebuilding before the swap.** At
  500,000 rows the maintenance is seconds; not worth a second code path.
- **Replacing the template in Plex's `Resources`** so PMS copies a shadow
  by itself. It is Plex's own file inside the image, and the mark is what
  tells a staged load from PMS's copy.

## As built (2026-11-04)

Both halves are built and tested; neither is released or deployed, and the
rig runs have not happened. The owner's "Begin".

**cluster-plex** (uncommitted at the time of writing; `pkg/plex/bootstrap`,
`cmd/manager`, `pkg/plex/api`, `pkg/plex/livetv`, `hack/guidediff`, the
chart and `docs/configuration.md`):

- `BuildShadow` marks every shadow with `ShadowMark` (`PRAGMA
  application_id`, `CLPX`); `IsShadow` reads it from the file's header;
  `BuildShadowTemplate` builds `Databases/.clusterplex/guide-shadow.db` at
  every start and `WriteShadowFrom` copies it over a load file in one
  rename. A test's "load PMS made" clears the mark, since the test helpers
  build their files with `BuildShadow`.
- The staging schema's state is its comment (`StagingAbsent`, `Clean`,
  `Loading`; `StagingStateOf`), `PrepareStaging` marks its clone clean, and
  `StageLoad` is the decision table of §"The manager on stage" as written.
  `SwapLoaded` analyzes and renames; `swapGuide` takes the live schema's
  comment, `loaded by PMS, swapped at <ns>` for a PostgreSQL load, which
  `guideLoadedAt` reads beside the copied-file comment so an older SQLite
  copy never undoes a swap. `RecoverLoads` is §"Recovery", keyed on the
  load file's inode the stage records in the mark (`fileInode`,
  `parseLoading`), which the guide file carries after PMS's rename. `PrepareGuides` drops every staging schema and
  load file at a start and builds the template; `SweepGuides` drops every
  `_next`, since no load runs at a start.
- The guide socket's `stage` verb (`stageLoad`, 45 s deadline) and the
  `swap` verb's branch on the mark (`swapLoad`); a clean `_next` is cloned
  ahead after each swap (`stageAhead`); the poll runs `recoverLoads` on
  each full pass. `plex.liveTV.guideLoadsInPostgres` (chart, default false)
  sets `PLEX_PG_GUIDE_LOADS=1`, only with the guide socket.
- The map guard: `plexapi.Client.Device` reads a device with its saved
  `ChannelMappings` (recorded fixture 09-device.json); `Converger.restoreMap`
  reads it back when the DVR's `refreshedAt` moves or every 10 min, never
  mid-load, and saves the tuner's pairing again when `mapDifference` finds
  a channel paired elsewhere, missing, or added by PMS, at most once an
  hour per device, Event `DVRChannelMapRestored`.
- `hack/guidediff`: the oracle, counts per table and the keyed diffs of
  `metadata_items`, `media_items` (with their parts), `tags` and `taggings`,
  exit 1 on a difference; its queries proven against two cloned guide
  schemas.

**plex-postgresql** (`a20a76e`, on top of `.47`; shipped inert in
`v1.3.17-clusterplex.48`, which a peer session tagged on top of it the same
afternoon for a second translator fix found on the incremental refresh, so
no release of its own is needed): `guide_load.rs` holds the flags, the stage
request at the open (`db_interpose_open.rs`, before the real open, 60 s),
the result-cache key (none for a load) and the statement savepoints
(`write_exec.rs`, `cached_write.rs`, `SAVEPOINT plex_pg_stmt`, on unless
`PLEX_PG_GUIDE_LOAD_SAVEPOINTS=0`); `epg_schema_for_path` answers
`epg_<uuid>_next` for a load and `is_guide_load_path` tells one from its
guide; `should_redirect_with` redirects a load only under the flag; 869 lib
tests pass and the cdylib links. The gating sits in `pg_config.rs` and
`guide_load.rs`, not `pg_route.rs`, which a peer session was changing for
`.47` (an IN list's ICU collation), `.48` (a doubled quote inside a
literal, `title='America''s Funniest Home Videos'`, read as the literal's
end, so the ORDER BY's `collate icu_root` reached the parser unrewritten)
and `.49` (the preprocess parenthesised a backtick-quoted identifier
before its collation correctly, but the AST quotes pass, `quotes.rs`'s
`fix_expr`, had no arm for `Expr::Collate`, so the backticks stood and
PostgreSQL received ``(parents.`title_sort`) COLLATE plex.icu_root`` in
the show-level episode lookup, `SELECT grandparents.*,
parents.*, metadata_items.* ... WHERE grandparents.id = ? ORDER BY
media_items.begins_at DESC, parents.`index`, parents.`title_sort` collate
icu_root, ...`; the refresh under `.48` got through all seven chunks and
died on that last lookup; the fix adds `Collate`, `Tuple` and `IS [NOT]
DISTINCT FROM` arms). The guide-loads shim's next release, should it
need one, is `.50`; the peer session owns `.49`. All three were found on
PMS's incremental refresh, which already sends the
loader's statements through the translator: the title lookup is `select
<every column> from metadata_items left join media_items ... where
library_section_id=? and metadata_type=2 and title='<title>' order by
absolute_index, "index", title_sort collate icu_root, id, media_items.width
desc, originally_available_at`, the title inlined as a literal. The loader
inlines values, so the rig's XMLTV guide should carry titles with
apostrophes, doubled quotes, backslashes and non-ASCII.

**The rig (kind-cluster-plex, namespace `media`, 2026-11-04 19:38Z on):**
the kind overlay at image `8459523` (one replica, its own machine identifier
-- the overlay's pinned one hashes to the owner's production identity, so a
rig Plex is never claimed), `hack/kind-postgres.yaml`, and `hack/hdhrprobe`
with a richer guide (100 channels, 24 two-hour programmes each on a fixed
UTC day, titles with apostrophes, doubled and double quotes, backslashes,
non-ASCII and XML specials, shows recurring across channels), its XMLTV DVR
made by hand through the PMS API as `hack/record-livetv.sh` makes one; an
unclaimed PMS 1.43.5 creates the DVR and loads the guide. Run 4 first (the
switch off): PMS loaded in 23 s, the manager copied 5,844 rows in 4.9 s
through the staging schema made ahead, 64 metadata items, 1,500 airings;
that guide is kept aside as `epg_off` for the oracle. Run 1 (the switch on)
staged in milliseconds (`cloned: false`) and then found the first defect:
PMS's loader makes each EPG section with an INSERT (id 1 from the
sequence) and renumbers it in place, `update library_sections set id=2
where id=1`; SQLite's next rowid is max+1, PostgreSQL's sequence handed out
2 again, the next section's INSERT was refused
(`library_sections_pkey`), and PMS ended the load at once: `Exception
inside transaction (inside=1)` at `MediaProviderEpgXmlTv.cpp:687`, `Failed
to load EPG`, the activity at 100%, the load file left for the start rule.
So the loader never continues past a refused statement, whatever the
savepoints leave usable (the open question above is settled: one refused
statement is one failed load, and on talos the map guard then restores the
map). Fixed in plex-postgresql `v1.3.17-clusterplex.50`
(`guide_load::id_assignment`, `resync_sequence`): a load's UPDATE that sets
`id`, or INSERT that names it, is followed by one `setval` to the greater
of `max(id)` and the sequence's last value, proven on the rig's staging
schema (an empty table leaves 2 at 2; an explicit 7 makes the next 8). The
shim logs its savepoint statements only when PostgreSQL refuses one, so
whether they ran is read from the rig's PostgreSQL with `log_statement =
'all'` on the next run, not from the shim's trace.

**Runs on `.50` (images `8f6b79f`, then `5b7779a` with the inode
recovery), 2026-11-04 20:21-20:40Z:**

- Run 1, the staged load end to end: the stage answered in milliseconds
  (`cloned: false`, the schema made ahead), PMS loaded into `_next`
  through the shim, the four section renumberings each raised the
  sequence, PMS renamed, the shim asked `swap`, the manager renamed the
  schemas in 666 ms (229 ms the second time) and cloned the next `_next`
  ahead; PMS wrote the DVR's `refreshedAt`, the grid answered from
  `epg_<uuid>` at the reopen, PostgreSQL logged no error, and the guide
  file on disk is the staged shadow (its mtime the stage's). PMS's
  "Running migrations. (EPG 1)" line is the phase's name: the shadow's
  `schema_migrations` is synced, so none ran.
- The oracle: `hack/guidediff` of the staged load (20:35Z) against a
  copy-path load four minutes later (20:39Z) is empty on `metadata_items`,
  `media_items`, `tags` and `taggings` (64, 1,400, 111, 2,212 each way).
  Only non-guide counts differ, from the clone's source: `schema_migrations`
  449 (the library's) against the template's 443, `preferences` 1 against
  2, the FTS node table. A first comparison 40 minutes apart differed by
  the first two-hour slot of every channel: PMS's own horizon drops an
  airing that ended more than about 18 h ago, so the two loads of an
  oracle must run minutes apart.
- Time: PMS's "database" time for the 100-channel, 1,400-airing guide was
  22.5 s (3.6 s on a fresh pod) through SQLite and 73.3 s staged, with
  the shim at INFO; 598 s with the shim at DEBUG, whose trace (3.3 GB in
  ten minutes) had the kind node's etcd timing out and the manager
  withdrawing the pod -- never run a load at DEBUG. PostgreSQL's own time
  for the staged load was under a second: the 73 s are round trips. The
  statement log (`log_statement = all`) shows why there are so many: the
  loader rewrites a show's taggings on every airing of the show (37,810
  tagging inserts and 671 deletes for 2,212 final rows), and every
  statement is a round trip through the shim, a write's three with its
  savepoint and a read's two with the `SET statement_timeout` before it,
  some 200,000 in all. So a staged load costs about 50 ms an airing on the
  rig over SQLite's, which for talos's 60,000-airing cloud load is 50
  minutes on top of the 21 the load takes today (whose "database"
  1,651 s are PMS's own per-programme fetches). plex-postgresql `.51`
  (next) drops the per-read `SET` on a guide's or a load's own connection,
  set to no timeout at its connect; the savepoints' cost is measured
  below, and they buy nothing, since the loader never continues past a
  refused statement.
- The savepoints ran: 6,740 `SAVEPOINT plex_pg_stmt` in PostgreSQL's log
  for the INFO run (the shim logs only a refused one).
- The recovery rule changed to the guide file's inode (above), after the
  rig showed a failed from-scratch load leaves the DVR's `refreshedAt`
  alone and talos that a cancelled incremental refresh moves it.

- Savepoints off (`PLEX_PG_GUIDE_LOAD_SAVEPOINTS=0`): 96.3 s against the
  73.3 s with them on, so they cost nothing measurable on this node, whose
  spread between runs (SQLite's own 3.6 s then 22.5 s) is wider than
  their share. PostgreSQL's statement log of that run settles where the
  time is: one loader connection ran 267,006 statements in 96.6 s, 24.7 s
  of them PostgreSQL's, the rest about 0.16 ms of client and round trip
  each; 91,229 were the `SET statement_timeout = 0` before every read,
  91,240 reads, 76,597 inserts (73,612 of them taggings), 5,230 updates,
  2,213 deletes, 227 transactions. The other four sessions ran a few
  hundred statements between them. plex-postgresql `.51` drops the
  per-read `SET` on a guide's or a load's own connection (a third of the
  statements) and turns the savepoints off by default.
- Run 8: 120 `GET /library/sections` at two a second through a staged
  load all answered 200.
- Run 3: after a swap PMS holds its ten handles on the guide file, ten on
  its WAL and one on the shm, the file the rename brought (its inode the
  load file's, marked `CLPX`), and none on a deleted file.
- Run 5: PMS killed (the pod deleted) 25 s into a staged load with 1,339
  airings in `_next` (its mark `loading since ..., file inode 53490497`):
  the start rule dropped the schema ("a staging schema at a start, when no
  load runs"), built the template, and the next load staged fresh and
  swapped in 93 s later.

**Not yet:** the late hook (covered by the recovery test; no knob on the
rig), the refused statement (the loader fails the load; the map guard
needs a cloud block, not on the rig), the cloud lineup (needs a claimed
server: the rig stays unclaimed, see the identity note); a timing run on
`.51`; talos with the value on, which is the owner's call on the load's
length: a from-scratch cloud load there is some 60,000 airings, so at the
rig's 50-70 ms an airing over SQLite's it runs an hour or more against
today's 21 minutes plus the 35 s copy, in the background while the old
guide serves, as today's does. The `.48`
deploy (talos rev 221, 2026-11-04) carries both halves inert, except the
map guard, which runs whatever the value: checked against talos's live DVR
32 before that deploy (PMS stores the map byte for byte as the tuner sends
it, 248 entries, every one enabled; `mapDifference` 0/0/0). The start rule
ran clean on that deploy: no `_next` schema to drop, the guide-shadow
template built, no load file to remove.
