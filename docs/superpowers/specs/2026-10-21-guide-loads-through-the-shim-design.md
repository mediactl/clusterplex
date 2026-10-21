# Live TV guide loads through the shim

Status: proposed, 2026-10-21. It replaces item 1 of the "As built" section of
`2026-10-20-epg-in-postgresql-design.md` (the manager copies each load) with a
load the shim writes into PostgreSQL itself. Everything else in that design
stands.

## Problem

PMS builds every guide load in `<guide>-loading.db` and renames it onto the
guide's file. The shim passes that file through to SQLite, and the manager
copies each finished load into the guide's schema (`ImportGuide`, polled every
30 s by `copyGuideLoads`). That costs:

- **A stale window.** After the rename, PMS reopens the guide, which the shim
  serves from PostgreSQL. The schema still holds the previous load for up to
  30 s plus the copy (3.2-3.3 s for 12,8xx rows on talos, 2026-10-21 16:25
  and 16:28Z). Then every row is renumbered under PMS: a load numbers its rows
  from 1. Whatever PMS does just after a swap reads the old guide.
- **Two type mappings.** The copy converts each column in Go (`kindOf`,
  `convert`) beside the translator's own rules, and the two can drift
  (shim .36 just changed how a bound time value is read).
- **Failures PMS cannot see.** A failed copy leaves PostgreSQL stale while
  PMS believes it swapped.
- **Mixed reads.** Until the next start the pod's guide file holds the real
  load, not a shadow, so the reads the shim lets reach SQLite disagree with
  the ones it answers from PostgreSQL.
- **No statistics.** The copy never runs `ANALYZE`, so a freshly swapped
  guide has none until autovacuum reaches its tables (talos: naptime 60 s,
  then only past each table's change threshold).

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
  each load to the 442 the library holds, deciding by
  `select version from schema_migrations order by version`.
- **The shim runs every statement in autocommit.** `BEGIN`, `COMMIT`,
  `ROLLBACK` and `SAVEPOINT` are skipped (`pg_config.rs`,
  `should_skip_sql_str`), so each INSERT of a load is its own commit. talos's
  library database runs `synchronous_commit = on`, so each one waits for a
  WAL flush.
- **The copy path already uses `COPY`**, in one transaction with user
  triggers held off. Its schema swap renames schemas, which locks none of
  their tables, so the cutover never queues behind a reader. Only the later
  `DROP SCHEMA … CASCADE` takes ACCESS EXCLUSIVE locks, on tables nothing
  new reads, under a 5 s `lock_timeout`.

## Design

### A staging schema per load

- A load fills `epg_<32 hex>_l<8 hex>`, eight random hex digits per load.
  The guide's live schema stays `epg_<32 hex>`. `epg_schema_for_path` derives
  only the live name from a file name, so no reader can ever bind to a
  staging schema.
- It is made by `<library>.clusterplex_clone_guide(src, dst)`, a PL/pgSQL
  port of `CloneSchema` that bootstrap creates or replaces at every start,
  owned by the `plex` role. Go's `CloneSchema` becomes a call to it, so
  `EnsureGuideSchema`, `ImportGuide` and the shim share one clone.
- The tables stay logged and keep every index (`LIKE … INCLUDING ALL`). PMS
  reads its own load while it writes it (tag lookups, for one), so indexes
  built afterwards would make each lookup a scan. An unlogged table is
  emptied by crash recovery and cannot become a live guide.
- The load's control connection (below) holds
  `pg_advisory_lock(hashtext('clusterplex-guide-load-' || staging))` for the
  load's life. A staging schema whose lock is free has no living load.

### The shim: opening `<guide>-loading.db`

1. `epg_load_for_path` matches `tv.plex.providers.epg.<provider>-<uuid>-loading.db`
   (and its `-wal`, `-shm` and `-journal`), giving the guide's live schema.
2. On the first open of a load (no load registered for the path), before the
   real open:
   - overwrite PMS's template copy with the template shadow named by
     `PLEX_PG_GUIDE_SHADOW`, and remove a stale `-wal`, `-shm` or `-journal`,
     all through the real libc calls. Otherwise the shim would prepare
     against the 2018 schema while PostgreSQL holds 442 migrations;
   - open the load's control connection, take its liveness lock, and run
     `clusterplex_clone_guide` into a new staging schema (about 3 s on talos's
     library, most of today's copy);
   - register the load: path, guide schema, staging schema, control
     connection, the file's device and inode, and an open-handle count.
3. Every open of the path while the load is registered binds to its staging
   schema. Each handle's own connection runs with `search_path` set to the
   staging schema and `synchronous_commit = off`. `handle_schema` and
   `apply_pg_session_settings`, which today derive the schema from the path,
   ask the load registry first, so a reconnect mid-load stays in staging.
4. **Fail closed.** With no `PLEX_PG_GUIDE_SHADOW` file, no PostgreSQL, a
   failed clone, or on macOS (no rename hook), nothing is registered and the
   file stays SQLite, as today. `ImportGuide` then copies the load (below).

PMS reads `schema_migrations` from staging, finds all 442, and runs none of
the ~320 migrations it runs on each load today. `synchronous_commit = off`
removes the WAL-flush wait from every autocommitted INSERT. A PostgreSQL
crash can lose at most the last ~0.6 s of commits (three times the default
`wal_writer_delay`), and a load that lost its connection is never swapped
(next section).

### The shim: the swap, at `rename()`

`rename(old, new)` is exported like `sigaction`. It passes straight through
unless `old` is a registered load and `new` is its guide's file. Then:

1. **Refuse a broken load.** Any handle still open, or any reconnect during
   the load (a gap of lost statements), skips the swap: drop the staging
   schema, log it, pass the rename through. PMS keeps its file swap; the
   previous guide stays served.
2. **Refuse an empty load.** If `SELECT EXISTS (SELECT 1 FROM <staging>.media_items)`
   is false, do the same. That is `ImportGuide`'s rule today.
3. **`ANALYZE`** each table in the staging schema, so the guide is never
   served without statistics.
4. **Swap**, in one transaction on the control connection:
   `SET LOCAL lock_timeout = '5s'`, `SET LOCAL synchronous_commit = on`,
   `pg_advisory_xact_lock(hashtext('clusterplex-guide-' || schema))` (the
   lock `ImportGuide` holds, so the two never swap at once), rename the live
   schema to `<schema>_o<8 hex>` if it exists, rename staging to the live
   name, and set the comment in `ImportGuide`'s format
   (`clusterplex: copied from a guide file modified at <now> (Unix ns)`), so
   a fallback copy of an older file never undoes this load.
5. Call the real `rename`.
6. Drop the replaced schema under a 100 ms `lock_timeout`; a reader still on
   it leaves it for the sweep. Close the control connection, which frees the
   liveness lock.

PMS's rename waits only for the `ANALYZE` (well under a second for ~13,000
rows) and the swap. If the swap fails, the staging schema is dropped and the
rename passes through: PMS reads its previous guide until the next load, as
when a copy fails today. Failing the rename instead (`EIO`, so PMS's own
"caught an exception trying to swap out the database" path runs) is decided
on the rig, once it shows what PMS does after a failed swap.

The other handles on the guide, on this pod and others, resolve
`epg_<uuid>` by name. After the commit their next statement replans against
the new schema. The shim's result cache keys on the schema name and expires
entries after 1 s.

### When the rename is not seen

A later PMS or Boost could rename through a call the shim does not hook.
PMS reopens the guide right after a swap, so that open checks: if a load is
awaiting its rename and `<guide>.db` now has the load file's device and
inode, the rename happened unseen. The shim swaps then (steps 1-4 and 6),
before the open binds, and logs at ERROR that the rename hook was bypassed.
A load still registered when the next load for the same guide opens was
abandoned, and its staging schema is dropped.

### A new DVR

A DVR's first load opens `-loading.db` like any other. At the rename there
is no live schema, so step 4 only renames staging to `epg_<uuid>`. PMS's
reopen then finds it, and the guide is in PostgreSQL from its first load,
with no SQLite phase and no restart.

### cluster-plex

1. **`ANALYZE` in `ImportGuide`**, on each staging table after `copyGuide`
   and before `swapGuide`. It ships on its own, in any image.
2. **The clone function** above, with `CloneSchema` calling it.
3. **The template shadow.** `PrepareGuides` builds
   `Plug-in Support/Databases/clusterplex-guide-shadow.db` with
   `BuildShadow`, taking `schema_migrations` from the library and seeding
   nothing. `shimenv.go` passes its path as `PLEX_PG_GUIDE_SHADOW`. The name
   matches neither the shim's redirects nor `copyGuides`' glob.
4. **A sweep in `PrepareGuides`**, under each guide's advisory lock:
   - staging schemas (`_l<hex>`) whose liveness lock is free, and the copy's
     `_next`;
   - replaced schemas (`_o<hex>`, the copy's `_old`), under the 5 s timeout;
   - live schemas of DVRs the library no longer lists (DVR 2's
     `epg_adfe4a6f…` on talos today). A DVR's row is written when it is
     created, before its first load, so a new DVR is never swept.
5. **`ImportGuide` and `copyGuideLoads` stay** as the fallback for a load
   the shim left in SQLite. After a shim swap the guide's file is the
   template shadow, which holds no airings, so the poll copies nothing. The
   shared comment format keeps the two writers in order.

### Considered and not taken

- **Upsert into the live tables, diffed by hash.** Every load numbers its
  rows from 1 across `metadata_items`, `media_items`, `media_parts`, `tags`
  and `taggings`, so an upsert by id would overwrite unrelated rows. PMS
  writes the rows; we have no payload to hash.
- **Swapping tables or a view.** `ALTER TABLE … RENAME` and
  `CREATE OR REPLACE VIEW` both take ACCESS EXCLUSIVE locks on objects
  readers hold, and queue every later reader behind them. A schema rename
  locks neither.
- **Partitions.** `DETACH PARTITION … CONCURRENTLY` cannot run in a
  transaction, so the cutover cannot be atomic, and PMS's tables are fixed.
- **Chunked `SKIP LOCKED` updates.** Nothing updates a guide in place.
- **A pre-cloned spare staging schema,** to save the ~3 s clone at open.
  Worth it only if the rig shows the open's wait matters.

## Testing

- **Shim unit tests:**
  - `epg_load_for_path`, with and without the `-wal`, `-shm` and `-journal` suffixes;
  - the load registry's lifecycle;
  - `handle_schema` and `apply_pg_session_settings` resolving staging for a loading path;
  - `rename` passing through for unrelated paths and for processes without a load;
  - the refusals (open handle, reconnect, empty load);
  - the inode check;
  - the comment matching cluster-plex's `importedComment`, a golden string both repos test.
- **cluster-plex, on the PostgreSQL harness:**
  - the clone function makes the same objects as today's `CloneSchema`;
  - `ImportGuide` leaves every staging table analyzed (`pg_stat_user_tables.last_analyze`);
  - the sweep drops a staging schema whose lock is free and keeps one whose lock is held;
  - it drops an unlisted DVR's schema and keeps the listed ones.
- **The standalone rig**, before talos:
  1. A load into a redirected `-loading.db` completes and runs no migration.
     Its rows match a SQLite load of the same feed.
  2. The swap happens in the rename hook, not in the safety net. The grid
     answers from the new schema at PMS's reopen.
  3. Load time through the translator, with and without
     `synchronous_commit = off`. The SQLite baseline on talos was about
     2.5 min, migrations included (2026-10-21, 00:22:02-00:24:34).
  4. How long PMS waits at the open for the clone.
  5. Whether a load or a swap calls `sqlite3_backup_init` or
     `sqlite3_deserialize`, both imported by `Plex Media Server`, through a
     logging interposer.
  6. A new DVR's first load lands in PostgreSQL with no SQLite phase.
  7. Failures:
     - PostgreSQL unreachable at the open: the load stays SQLite and `ImportGuide` copies it;
     - the advisory lock held past 5 s: the previous guide stays;
     - PMS killed mid-load: the next start's sweep drops the staging schema.
  8. What PMS does when `rename` fails with `EIO`, to settle the swap-failure
     rule above.
  9. The usual concurrent pass during a guide load: browsing, search, and
     100+ `/library/sections` probes answering 200.

## Rollout

1. `ANALYZE` in `ImportGuide`, in the next cluster-plex image.
2. The clone function, with `CloneSchema` calling it (cluster-plex).
3. The shim: load registry, `rename` hook, safety net. Released as the next
   `v1.3.17-clusterplex.N`, with the cluster-plex image that sets
   `PLEX_PG_GUIDE_SHADOW` and sweeps.
4. The rig runs above, then talos. No PMS restart is needed beyond the image
   rollout itself.

## Risks

- **A wrong schema on the hot path.** A loading handle that resolved the
  live schema would write a load into the guide PMS is reading. The registry
  lookup must come before the path-derived schema everywhere a guide
  connection's `search_path` is set, and tests must interleave loading and
  live handles on one thread.
- **The rename hook runs in every process the shim loads into**, the
  Transcoder and Scanner included. It must cost one atomic load when no load
  is registered.
- **Statements PMS rolls back stay in staging,** because the shim runs in
  autocommit. The library already has this property. A load PMS abandons is
  never swapped, but a load it rolls back partly and still renames would be.
  The rig's row comparison (check 1) is the guard.
