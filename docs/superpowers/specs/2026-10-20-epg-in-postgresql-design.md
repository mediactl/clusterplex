# Live TV's guide in PostgreSQL

Status: approved by the owner, 2026-10-20 ("Do the full PostgreSQL move").
Built 2026-10-21, with changes recorded under "As built" below.

## Problem

Plex keeps each XMLTV DVR's guide in its own SQLite file:
`Plug-in Support/Databases/tv.plex.providers.epg.xmltv-<dvr uuid>.db`. The
shim redirects only `com.plexapp.plugins.library.db` and `…library.blobs.db`
(`plex-postgresql/rust/plex-pg-core/src/pg_config.rs:49`). The guide
therefore sits on the pod's emptyDir, beside the library's shadows.

- **A replaced pod starts with no guide.** Talos rev 69 replaced plex-0 at
  22:54Z. Afterwards the guide grid's `totalSize` was 0, and the guide file
  held 243 channel `tags` and no airings. Every live session read
  "Unknown Airing".
- **Nothing reloads it.** The Butler is forced off. The converger's Lease
  note (`livetv.clusterplex.io/saved`) records the guide hash it last
  reloaded for, and that hash still matches.
- **Each pod has its own copy.** With several Plex pods, each needs its own
  guide load, and the converger reloads only the lease holder's.

## What was found (talos, PMS 1.43.4)

- **The guide database has the library's schema.** It has the same 72 tables
  (plus SQLite's virtual fts4 and spellfix tables), 162 indexes and 8
  triggers. Its `schema_migrations` holds the same 442 versions, up to
  `500000000001.231`, that `schema/plex_schema.sql` records. A guide is
  therefore a second instance of the library schema, and the shim's
  translation already covers its statements.
- **Its file name is the DVR's own uuid.** `GET /livetv/dvrs` lists DVR 2 as
  `uuid adfe4a6f-…`, and that is the file's uuid. The library, already in
  PostgreSQL, records every DVR, so the guides that should exist can be
  listed at start.
- **PMS opens the guide on connections of its own.** The PMS binary and its
  libraries hold no `ATTACH` (only libsqlite3's grammar does), so no SQL
  joins the library and the guide.
- **The shim has one schema.** `PLEX_PG_SCHEMA` is set on every pooled
  connection (`SET search_path TO <schema>, public`).
  - Statement caching is safe across schemas. Each connection has its own
    cache, and PostgreSQL re-parses a prepared statement when `search_path`
    changes, so a library statement is never run against a guide.
  - The result cache (`pg_query_cache.rs`) is keyed by SQL and parameters
    only. It must also carry the schema.
- **Some database objects belong to a schema.** The four compatibility
  functions live in `public`, which every schema shares. The 2026-10-16 FTS,
  collation and index migration creates `plex.fold`, `plex.nocase`,
  `plex.icu_root` and indexes inside `plex`.
- **The dump qualifies every name `plex.`** (474 times). Its one data block
  is `COPY plex.schema_migrations`, whose rows contain strings such as
  `tv.plex.providers.epg.cloud`. Renaming the schema must therefore touch
  statements only, never copied rows or quoted strings.

## Design

### Schema per guide

Each guide database `tv.plex.providers.epg.<provider>-<uuid>.db` is a schema
`epg_<uuid without dashes>` in the library's database: a full instance of the
library schema.

- It is loaded from `plex_schema.sql`, its seed and column types, then the
  2026-10-16 migration, all renamed from `plex` to the guide's schema.
- The rename is quote-aware, over statements only. `ParsePSQL` already
  separates `COPY` rows and knows quoting.
- Like the library, it records every migration, so PMS runs none.

### The shim (plex-postgresql)

1. **Redirect.** `should_redirect_str` also matches
   `tv.plex.providers.epg.` files and derives the schema name from the
   file's uuid. A file with no uuid is never redirected.
2. **A schema per handle.** A redirected handle records its schema.
   - When a pooled connection is taken for a handle whose schema differs from
     the connection's current `search_path`, the shim issues
     `SET search_path TO <schema>, public` first. The connection tracks its
     current schema, so the switch costs one round trip.
   - Nothing that runs inside a transaction may switch the schema. A
     transaction's statements come from one handle, so this holds by
     construction; a debug assertion guards it.
3. **Result cache.** The schema becomes part of the key.
4. **Fail closed to SQLite.** If the guide's schema does not exist when PMS
   opens the file (a DVR created since the last start), the handle is not
   redirected, and the file stays real SQLite as today. One log line names
   it.
5. **The process check.** The process-name heuristic reads argv[0]'s base
   name, not the last `/` of the whole command line (a Scanner started with
   a path argument would otherwise lose the shim).

### cluster-plex bootstrap (before PMS starts, every start)

1. List the DVR uuids from the library: `media_provider_resources` rows for
   DVRs (the type and identifier are to be confirmed against the dump).
2. For each, run `InitSchema` into `epg_<uuid>` with the rename. A populated
   schema is left alone.
3. Build the guide's shadow file from `sqlite_schema.sql`, plus its
   `schema_migrations`, the way `BuildShadow` builds the library's. PMS opens
   that file, and the shim answers from PostgreSQL.
4. Record which schemas this start created. The converger reloads those
   guides once (below).

### Guide loads: one writer

- The guide is now shared, so only the lease holder loads it. The converger
  already reloads only through its own PMS. A schema created at bootstrap is
  empty, so the converger treats it as needing a reload: it reloads any DVR
  whose guide holds no airings, besides the hash rule. That also covers this
  incident.
- **PMS starting a load by itself is the open question.** Per-pod guide
  refreshes on start would race on the shared schema. With the Butler off,
  none was seen; the test below must confirm it.

### A DVR created at runtime

The converger creates the DVR, and PMS creates its guide as real SQLite (rule
4 of the shim). The converger then creates the schema and requests one PMS
restart, the existing `restartOwnPMS`. Other pods restart through the
existing DVR-change round. At that start the shadow is built and the guide is
reloaded into PostgreSQL. A DVR's creation is rare (once per block).

### Rollout on talos

- One shim release, plex-postgresql `v1.3.17-clusterplex.35`, and one
  cluster-plex image.
- At its first start, bootstrap creates `epg_adfe4a6f79d0413593d5edf0235ec02b`,
  and the converger reloads DVR 2's guide into it.
- Any older guide SQLite file is ignored (the emptyDir goes with the pod).

## Testing

- **Shim unit tests:** redirect matching, schema derivation, the result-cache
  key, the fail-closed open and the argv[0] check.
- **cluster-plex tests:**
  - the quote-aware rename against the real dump (golden: no `plex.` left
    outside strings, and `COPY` rows untouched);
  - `InitSchema` into a second schema on the PostgreSQL harness, where both
    schemas coexist;
  - the shadow builder for a guide.
- **The standalone rig** (`plex-postgresql/docker-compose.standalone.yml`,
  PMS against PostgreSQL), before talos:
  - create an XMLTV DVR and load a guide;
  - check that the grid answers from `epg_<uuid>`;
  - check library browsing alongside it, under concurrent load;
  - restart PMS and check the guide is still there without a reload;
  - check that a fresh shadow and a new pod keep it.

## Risks

- **The schema switch sits in the shim's hottest path,** and a mistake would
  read one schema's rows as another's. The tests must interleave library and
  guide handles on one thread.
- **A guide load writes through the translator.** About 2,800 programmes
  today; iptv-epg.org's guide would be about 26,000. Time it on the rig.
- **The 2026-10-16 migration's `plex.`-scoped objects** (collations, `fold`,
  the trigram indexes) must exist per guide schema, or full-text guide
  search fails.

## As built (2026-10-21)

The build departs from the design above in five places. Where they
disagree, this section is right.

1. **PMS's loads never reach the shim.** A full guide refresh copies Plex's
   bundled `Resources/com.plexapp.plugins.library.db` to
   `<guide>-loading.db`. PMS migrates and fills that file, then renames it
   onto the guide's file. On talos's plex-0 the guide file's birth time was
   00:22:02.290 on 2026-10-21, the moment of the log's "Installing Library
   Database … -loading.db". Its `-wal` and `-shm` were born at 00:24:34, when
   the load ended. clustarr-c2 found this.
   - The shim goes by file name and never redirects `-loading.db`, so the
     load stays in SQLite.
   - The manager copies each load into the schema instead:
     `plexbootstrap.ImportGuide`, called from `cmd/manager/guides.go`
     (`copyGuideLoads` runs every 30 s on every pod, and `guideEmpty` copies
     before it judges).
   - The copy runs in four steps. A staging schema `epg_<uuid>_next` is
     cloned from the library. Every table the file and the schema share is
     copied in, each column through SQLite's `CAST` to its PostgreSQL kind,
     with user triggers held off. The sequences are reset past the copied
     rows. Finally the schemas are renamed: the guide schema becomes
     `_old`, and `_next` takes its name.
   - Renaming a schema waits on no reader, so PMS never stalls behind a swap.
     `_old` is dropped under a 5 s lock timeout, or at the next look.
   - The schema's comment records the copied file's modification time. Only
     a file that holds airings and is newer than that record is copied.
   - On talos's real guide (2,385 programmes, 7,157 airings) the copy takes
     3.7 s, most of it the clone.
   - `PrepareGuides` copies a load the pod's file still holds before the
     shadow replaces it.
2. **No PMS restart when a DVR is created.** The converger no longer asks
   for one (`GuidesInPostgres` is gone). A new DVR's guide stays SQLite
   until its first load is copied into a new schema. Other pods read that
   schema from their next start, which the DVR-change round already causes.
3. **The schema is copied, not replayed** (`CloneSchema`). It gets the live
   library schema's structure: tables `LIKE … INCLUDING ALL`, its own
   sequences with their defaults pointed at them, views, and triggers.
   Functions, collations and extensions stay `plex.`'s, which the
   translator names in full. The `spellfix_vocab_add` triggers are left
   out, because they write into `plex.spellfix_vocab` by name.
4. **The shim gives each guide handle its own connection,** instead of
   switching schemas on pooled ones (plex-postgresql 6677276):
   - only the library and blobs use the shared pool;
   - a guide's dedicated connection is set to `search_path` `epg_<uuid>`,
     and so is each of its reconnects;
   - the result cache key carries the schema;
   - a guide whose schema is missing stays SQLite (fail closed).
5. **Shim rule 5, the argv[0] check, was built and then backed out**
   (plex-postgresql 44b3c47). Nothing here needs it, and it changes which
   processes get the shim, which nothing has looked into on PMS. The check
   still reads the text after the command line's last `/`, so a test harness
   must not pass the shim a path argument: such a process passes everything
   through to SQLite. talos's subreaper is a static binary, and the shim
   never loads in it.

Not yet done: the standalone rig run in "Testing" above (a PMS load
against PostgreSQL), and talos.

