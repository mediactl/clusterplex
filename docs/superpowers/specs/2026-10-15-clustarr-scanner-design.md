# The clustarr scanner: Plex's library as a projection of clustarr

Date: 2026-10-15. Status: approved design, not built.

## 1. Purpose

Plex's `Plex Media Scanner` walks the library on disk, creates a row for
every file it finds, matches each to an item through the metadata provider
and probes it with its own analyzer. For a clustarr-backed library every
one of those steps repeats work clustarr has already done and recorded on
the MediaFile: the path, size and container, every stream, the chapters and
markers, the item it belongs to and that item's Plex id. The scanner also
cannot be told what changed: cluster-plex asks it to rescan a folder, and a
renamed or transcoded file becomes a new row beside a trashed one.

This design replaces the scanner for provisioned libraries with a
reconciler in cluster-plex that writes Plex's rows straight from the
MediaFile. "Scan Library Files" becomes a comparison of Plex's rows with
clustarr's records: seconds, no file I/O, no probe. Plex keeps what it does
well: descriptive metadata and images, fetched once per new item from
clustarr's provider; watched state, playlists and hubs, untouched.

## 2. What exists today

- Every `Plex Media Scanner` run already reaches the cluster-plex manager:
  the image links Plex's helper binaries to the shim, which forwards the
  argv, env and cwd as an `ExecRequest` (`cmd/shim`, `pkg/remoteexec`). The
  dispatcher inspects only `Plex Transcoder`; the scanner runs its `.real`
  binary unexamined.
- `pkg/plexseed` writes `media_items`, `media_parts`, `media_streams` and
  marker `taggings` straight into PostgreSQL, matched by path, on the lease
  holder only, under ADR-0006. Its rows are stamped `cp:source=clustarr` and
  `cp:probeHash` in `extra_data`. It never sets `media_analysis_version`, so
  Plex's analyze replaces its rows when it runs; it never writes HDR.
- `pkg/clustarrwatch` watches MediaFiles, Movies, Series and Episodes with
  a trimmed informer, maps clustarr paths to Plex paths (`paths.go`), and on
  a MediaFile change queues `POST /library/sections/{id}/refresh?path=<dir>`
  for the old and new folder, 30 s debounced; on an item change it queues
  `PUT /library/metadata/{id}/refresh`, looking the id up by the custom
  guid. With clustarr's default `plexGuids: true` the stored guid is
  `plex://…`, so that lookup finds nothing and the refresh is silently lost.
- Plex's own scan scheduling (`FSEventLibraryUpdatesEnabled`,
  `ScheduledLibraryUpdatesEnabled`) and Butler tasks are forced off
  (`pkg/plex/prefs/required.go`); scanning happens through the maintenance
  `refresh` CronJob and the watcher's folder refreshes.
- PostgreSQL is the only copy of `library.db`: the shim routes every
  statement there and each pod's SQLite is an empty shadow. A committed row
  is visible to Plex on its next query.
- clustarr records per MediaFile: `spec.path`, `spec.sizeBytes`,
  `status.probeHash`, `status.mediaInfo` (container, codecs, profile,
  dimensions, fps, bitrate, HDR, every audio and subtitle stream, chapters),
  `status.markers`; per item the Plex id (`status.metadata.externalIDs.plex`,
  `plexSeasons`, Episode `status.plexID`).

## 3. Decisions

| Question | Decision |
|---|---|
| How much of a Plex entry do we write? | The structure: directories, parts, media items, streams, markers, and the item rows with their guid and parent chain. Plex fills descriptive fields and images through its `refresh` of each new item. A later design may take over the descriptive layer; this one does not. |
| A renamed or transcoded file | The same `media_items` row, updated in place. No new row, no trash entry, no second version. |
| "Scan Library Files" | A reconcile of the section (or of one folder for `?path=`) against clustarr's MediaFiles. Never a clustarr rescan, never a no-op. |
| Where it runs | cluster-plex's lease holder, fed by the informer `clustarrwatch` already runs. Not from NATS, not from clustarr. |
| Spike | Deferred: the open questions in §10 are the implementation plan's first task. |

## 4. Identity

- **Media:** one `media_items` row per MediaFile. The row's `extra_data`
  carries `cp:mediaFile=<MediaFile uid>` beside the seeder's keys. Lookup is
  by UID first; a row without a UID whose `media_parts.file` equals the
  mapped path is a row Plex's scanner made before this design, adopted and
  stamped on first contact.
- **Items:** `metadata_items` rows are keyed by guid, as Plex keys them:
  `plex://<type>/<24hex>` when clustarr holds the Plex id, else the custom
  `tv.plex.agents.custom.clustarr.<movies|tv>://<type>/<ratingKey>`. This is
  the provider's own rule (`ui/plex/plexguid.go`), so the guid we store is
  the guid Plex would have stored. The guid rule lives in one function used
  by every lookup and insert, which also fixes the watcher's lost refresh.
- **Chain:** movie (`metadata_type` 1); show (2) → season (3) → episode (4).
  A season's guid is the series' Plex season id when known, else the custom
  `<seriesUID>-sNN` ratingKey form the provider uses. Items with no file get
  no row.
- **Scope:** only the sections `plex.libraries` provisions, matched by the
  section's `id` recorded at provisioning (today's match by display name is
  replaced, so a renamed library keeps working). Every other section's
  scanner run goes to the real binary untouched.

## 5. Triggers

| Source | Arrives as | Becomes |
|---|---|---|
| MediaFile add/update/delete under a provisioned root | informer event | an apply (§6) or delete (§7) of that file, batched on a 2 s tick |
| Movie/Series/Episode `status.metadata` change (title, Plex id) | informer event | an apply of the item's rows, then Plex `refresh` of the item |
| Scan Library Files, API `/refresh`, API `/refresh?path=`, the `refresh` CronJob | `Plex Media Scanner` run through the shim | a reconcile (§8) of the section, or of the folder; exit 0 when done |
| 6-hour resync | timer | a reconcile of every provisioned section |

The watcher's folder-refresh scheduling is removed for provisioned
sections and kept for anything else. The seeder's `SeedKey` batching is
folded into the apply queue.

## 6. Apply, one file, one transaction

1. **Resolve the item** from the informer caches: the MediaFile's
   `spec.mediaRef` to a Movie, or to an Episode and its Series. No item yet
   (an import whose item is still pending) means skip; the next change
   retries. Never a guessed row.
2. **Ensure the item chain** by guid, top down. A missing row is inserted
   with `guid`, `metadata_type`, `parent_id`, `library_section_id`, `title`,
   `title_sort`, `index` (season or episode number), `year`, `added_at`,
   `created_at`, `updated_at`, and the item is queued for a Plex
   `PUT /library/metadata/{id}/refresh`, which makes Plex fetch summary,
   art and tags from clustarr's provider. An existing row is left alone:
   Plex owns its descriptive fields.
3. **Ensure the folder:** a `section_locations` row for the root, and
   `directories` rows for each path element below it, created on demand.
4. **Upsert the media row** keyed by UID: `media_parts` (`file`, `size`,
   `duration`, `directory_id`), `media_items` (`container`, `video_codec`,
   `audio_codec`, `width`, `height`, `duration`, `bitrate`,
   `frames_per_second`, `display_aspect_ratio`, `audio_channels`,
   `color_trc` from the probe's HDR, `metadata_item_id`,
   `section_location_id`, `media_analysis_version` at Plex's current value
   so Plex never analyses the file).
5. **Streams and markers:** `plexseed`'s row code, called in the same
   transaction, extended with the fields it never wrote (HDR, commentary,
   bitmap subtitles, `open_subtitle_hash` when clustarr has it). The
   seeder's "leave rows that are not ours alone" rule stays for streams.
6. **Stamp** `cp:mediaFile`, `cp:probeHash` and `cp:seedKey` so a later
   reconcile can tell a current row from a drifted one without reading
   every column.

A path change is step 4 on the existing row. Play position and watched
state are keyed by guid in `metadata_item_settings` and are never touched.

A changed path or probe on a row found by its stamp (2026-10-20) also sets
the item's `updated_at`, which clients read as `updatedAt`, and queues the
item for the post-commit refresh, which sends open clients Plex's timeline
"updated". Plex's refresh alone does neither stamp: with the provider's
answer unchanged it writes only `refreshed_at` (talos, Before Sunrise after
its transcode). A markers-only change, a chain change and an adoption do
neither.

## 7. Delete

A deleted MediaFile soft-deletes its `media_parts` and `media_items`
(`deleted_at`), as Plex's scanner does. A `metadata_items` row left with no
live media is soft-deleted, and so on up the chain. The `empty-trash`
CronJob empties them as today, so a mistaken delete shows in Plex's trash
before it is gone.

## 8. Reconcile

Input: the informer cache filtered to the section's mapped roots, or to
the folder. Database side: every live row of the section with its
`cp:mediaFile` stamp and `cp:seedKey`. Then:

- a MediaFile with no row, or whose seed key differs from the stamp, gets
  the apply of §6;
- a row whose UID names no MediaFile is deleted as in §7;
- a row with no UID whose path matches a MediaFile is adopted (§4);
- a row with no UID and no matching path is a file Plex scanned that
  clustarr does not know: left alone and counted, never deleted.

A full reconcile reads the section's rows in one query and the cache in
memory; on the owner's library (about 14,000 files) that is seconds.

**Multi-episode files (2026-10-20).** A file holding several episodes is on
each of them, as Plex's own scanner puts it: one media row and part per
episode, all at the one path. The row on the episode `spec.mediaRef.name`
names carries the file's UID; the row on each other episode in
`spec.mediaRef.keys` carries `<uid>#<episode>` (`plexproject.CoveredUID`).
Adoption prefers a stamp-less row already on the change's own item, so
each episode takes its own of Plex's rows. Before, only the first episode
was projected, and a transcode that renamed the file left the others on
parts whose file was gone (talos: 11 such episodes).

**Gone files (2026-10-20).** Plex removes no gone file on a refresh, and
its scanner no longer runs here, so the reconcile sweeps after it
(`plexproject.Projector.Sweep`, the section or the scan's folder), and so
does every item Plex finishes refreshing: the watcher follows Plex's
`/:/eventsource/notifications` for library timeline entries in state 5,
which every client's refresh produces, TLS ones the proxy cannot see
included, and sweeps the item and those under it 10 s after the last.

- Only a row with no `cp:mediaFile` stamp, one Plex's scanner made, is
  removed: soft-deleted with its parts as §7 does, stamped `cp:missing`, its
  item trashed if nothing is left, its item's `updated_at` moved if it keeps
  a file. The `empty-trash` CronJob purges it.
- A projected row whose file is gone is never removed -- clustarr's
  MediaFile is wrong, and the next apply would put the row back -- only
  counted (`clusterplex_projection_missing_files_total{action="projected"}`)
  and logged.
- Nothing under a location root that does not list at least one entry is
  judged (an NFS mount that is not there looks like that); a stat that
  errs leaves its row; one that does not answer in 10 s stops the sweep.
- A `cp:missing` row whose file is back is put back, with its parts, its
  item and every parent it trashed, until the trash is emptied.

## 9. Ordering and safety

- All writes run on the lease holder, through one queue per section.
  Continuous applies, a scanner-triggered reconcile and the resync wait
  their turn, so a scan never overlaps the apply stream. Each file is its
  own transaction: a failure skips one file and is retried at the next
  change or reconcile.
- The scanner interception waits on that queue and returns the exit code
  of the reconcile, so Plex's "Scanning" activity ends when the rows are
  written.
- For provisioned sections the only writer of these tables is this
  reconciler: Plex's scanner no longer runs, Plex's scan scheduling is
  already forced off, and the Butler analyze task is held off by a
  `required` preference the same way. `media_analysis_version` is written
  at Plex's current value so an analyze that slipped through finds nothing
  to do.
- ADR-0006's bound on direct writes widens to `metadata_items`,
  `directories` and `section_locations` for provisioned sections; a new
  ADR records that, superseding 0006's "media tables only".

## 10. Open questions, the plan's first task

Answered by a live capture on talos before any writer is built:

1. The scanner's argv for a section scan, a `?path=` scan and the
   maintenance refresh, so the interception's parser matches reality.
2. Whether Plex's `refresh` of a `metadata_items` row it did not insert
   fetches from the provider and fills it, or needs `hash`, `changed_at`
   or `media_item_count` set first; and what Plex's current
   `media_analysis_version` is.
3. Whether connected clients get an "added" notification without the
   scanner, or need a nudge.

If 2 fails outright, the descriptive layer must be written by cluster-plex
as well; that becomes a design of its own and this one's §6 step 2 stays
as the structural part of it.

## 11. Testing

- Package tests against a real PostgreSQL loaded with the shim's
  `plex_schema.sql`, under `CLUSTERPLEX_TEST_POSTGRES_DSN` as the seeder's
  and bootstrap's tests already run: apply, path change in place, delete up
  the chain, adopt by path, reconcile's four cases, the unknown-row rule,
  the guid rule for both forms.
- The argv parser against the argv captured in §10.
- The queue: a scan during a stream of applies never interleaves.
- The `fields.go` contract test against clustarr's CRD YAML (§12).
- Falsification per rule: remove it, watch its test fail by name.
- End to end on talos once built: a transcode swap, a rename, a delete and
  a button press, each checked in Plex's UI and in the database.

## 12. Dependencies

The direction stays one way: clustarr is the source, cluster-plex and
Plex are consumers.

| From | To | How |
|---|---|---|
| cluster-plex | clustarr | the Kubernetes API only: get, list and watch on MediaFiles, Movies, Series and Episodes (the `plex-clustarr-reader` Role). Never a clustarr process. |
| Plex | clustarr-ui | HTTP, the metadata provider, on an item refresh. |
| cluster-plex | Plex | the database (§6-§8) and the PMS API (`refresh`). |
| clustarr | cluster-plex or Plex | nothing. |

No webhook: clustarr pushing changes to a cluster-plex endpoint would
make clustarr know cluster-plex's address, carry its auth and retry its
outages, and cluster-plex would still need a list for the reconcile. The
watch gives push delivery, replay after either side restarts and the list,
with the apiserver as the broker.

Two couplings remain and are held by tests and by degrading, not failing:

- **Schema:** cluster-plex reads clustarr's fields through
  `clustarrwatch/fields.go`. A contract test loads clustarr's CRD YAML and
  checks that every path in `fields.go` exists, so a renamed field fails a
  test rather than silently projecting nothing.
- **Availability:** the informer waits for the CRDs; a `refresh` Plex
  cannot complete (clustarr-ui down) is requeued with backoff. A clustarr
  that is gone leaves Plex's rows as they are.

Should clustarr ever need Plex's state (watched status for a list, say),
that is a clustarr agent reading Plex's API, never cluster-plex writing
clustarr's resources; ADR-0019 already forbids the latter.

## 13. Out of scope

Descriptive metadata and image bundles (Plex's refresh keeps them); files
clustarr does not know; non-provisioned sections; Live TV and DVR rows;
music, books and photos.
