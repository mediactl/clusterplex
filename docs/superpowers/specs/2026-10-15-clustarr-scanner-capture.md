# Capture: what Plex 1.43.4 does on talos (2026-10-15)

Plan task 1 of `2026-10-15-clustarr-scanner.md`. Observed on `admin@talos`,
namespace `clustarr-system`, Plex 1.43.4.10903, one pod (`plex-0`), the
library at 15,411 live media rows.

## Scanner argv

**Library scans never run the binary.** Three `POST /library/sections/1/refresh`
requests (a folder scan, a whole-section scan, a forced folder scan) were
answered 200 and section 1 reported `refreshing: true` for under three
minutes (started 21:48:06 UTC, done by 21:51:04), while an ephemeral
container polling `/proc/*/cmdline` every 0.1 s saw no `Plex Media Scanner`
process and the manager logged no `job started`. Plex 1.43's "Plex Movie"
and "Plex TV Series" scanners run inside the server process. The shim
cannot intercept a library scan.

**The binary is used for analysis only.** The only scanner logs are
`Plex Media Scanner Analysis*.log`, each one invocation:

    Plex Media Scanner.real --analyze --log-file-suffix Analysis --no-thumbs --item 14371

Plex runs it for an item it has just scanned (the eventsource showed
`mediaState: "analyzing"` on freshly scanned episodes) even with every
Butler analysis task off. One run at 21:46:40 failed on an NFS file
("Failed to successfully analyze part 15995"). 3,604 live media rows sit
at `media_analysis_version = 0`.

## Media analysis version

    SELECT media_analysis_version, count(*) FROM plex.media_items WHERE deleted_at IS NULL GROUP BY 1
    8 | 11807
    0 | 3604

`MediaAnalysisVersion = 8`.

## Schema facts the plan's SQL must follow

- Every `*_at` column (`added_at`, `created_at`, `updated_at`,
  `refreshed_at`, `deleted_at`) is a bigint of Unix seconds, not a
  timestamp: an insert with `now()` fails with "column added_at is of
  type bigint but expression is of type timestamp with time zone".
- `directories` has one root row per section location with `path = ''`
  and `parent_directory_id` NULL (id 1 for section 1); every folder row
  hangs off it with a path relative to the location root
  (`127 Hours (2010) {tmdb-44115}`).
- Sections: 1 `Clustarr Movies` (location 1, `/media/movies`), 2
  `Clustarr TV` (location 2, `/media/tv`).
- Item rows Plex wrote carry `hash` (a 40-hex digest of the provider's
  answer), `changed_at`, `refreshed_at` and a `pv:*`-keyed `extra_data`.
- 488 of 15,411 media rows carry the seeder's `cp:source` stamp.

## Refresh of a row Plex did not insert

1. **An existing row with its summary emptied and `refreshed_at` NULL:**
   `PUT /library/metadata/7/refresh` (and `?force=1`) set `refreshed_at`
   and left the summary empty. Plex compares the provider's answer with
   `metadata_items.hash` and writes nothing when they match.
2. **The same row with `hash` NULL:** the refresh refilled the summary
   (406 characters) and wrote a new hash within 5 seconds. Plex's debug log
   showed the agent calling clustarr's provider: `POST …/matches`,
   `GET …/library/metadata/<plex id>`, `…/extras`, then `/art/...`.
3. **An inserted row** (section 1, type 1, `plex://movie/5d77684554c0f0001f30556f`
   for 35 Up, which clustarr holds with a 200-character overview, genres
   and a poster, and which has no file; `hash` NULL, `media_item_count` 0,
   epoch timestamps, `extra_data` ''): `GET /library/metadata/16423` was
   200 at once; the refresh within 5 seconds wrote `tags_genre`
   (`Documentary`), 25 `taggings`, `hash`, `refreshed_at` and a new
   `changed_at`, but **no summary and no thumb**. The row had no media; 2
   above, with media, got both. The projection inserts item and media in
   one transaction, so the live case is 2. A show or season row (never
   with media of its own) is unverified: Task 15 checks a new series.
   The row was deleted afterwards (taggings first). An older soft-deleted
   row for the same guid (id 12) was neither revived nor merged.

So an inserted item row needs `hash` NULL and `refreshed_at` NULL, and
`media_item_count` is not required for the refresh.

## Notifications

With `/:/eventsource/notifications` open during 3, no `timeline` event
named the inserted or refreshed item. Scans emit `timeline` entries per
item (`state: 1`, then `mediaState: "analyzing"`); a refresh of an
inserted row emits nothing. Clients learn of a projected item when they
next ask (hubs and library views are database queries). No nudge is built;
revisit if a client visibly lags.

## Consequences for the plan

- Tasks 3 and 12 cannot intercept library scans. The reconcile needs
  another trigger (owner's call, pending). The analyze invocation can be
  intercepted and, for a provisioned section's item, answered with exit 0,
  since the projection writes the streams.
- Tasks 6-9: epoch-second timestamps; `directories` root row; inserted
  item rows with `hash` NULL.
- Task 4: `MediaAnalysisVersion = 8`.

## As built (talos, 2026-10-16, cluster-plex 23aa7e0, clustarr revision 23)

- The first resync adopted the library: Movies in 45 s (837 applied, 835
  adopted, 6 unknown: Plex's own optimised versions), TV in 16 minutes
  (10,105 applied, 10,094 adopted, 1,161 unknown: files clustarr has not
  imported), nothing deleted, nothing failed. Afterwards 10,941 live rows
  carry `cp:mediaFile` and a seeded probe, against 10,942 MediaFiles.
- A direct `POST /api/v1/scan?section=1` with the owner's token answered
  in 9.7 s: 226 applied (files whose markers clustarr had refreshed
  meanwhile, so their seed key moved), 0 adopted, 6 unknown.
- An adopted movie ("20th Century Women") is served by Plex with
  clustarr's streams -- HEVC video, AAC English audio, SRT and two PGS
  subtitles -- its row at `media_analysis_version` 8, `color_trc` bt709;
  `/hubs/sections/1` answers 200.
- TV folders are stored as full relative paths under one root row per
  section (`1923 (2022) {tvdb-416491}/Season 1`), as `EnsureDirectory`
  writes them; show and season rows carry `media_item_count` 0, as
  `countMedia` leaves them.
- 318 live rows had a NULL `extra_data`; the reconcile read and adopted
  them (review I5).
- Not yet observed live: a transcode swap, a delete, a brand-new import
  and the Plex Web button. The talos release pins the published
  cluster-plex chart 0.4.0, which has no `scan-route`, so the proxy's
  divert is off there until the chart is published; `/api/v1/scan`
  stands in. No `--analyze` ran in the window, so the skip is unproven
  live.
