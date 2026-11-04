# The cloud guide, loaded by the manager (design, concept stage)

**Status (2026-11-04 15:00Z): a concept the owner asked to have written down,
not approved to build.** The owner pivoted the same hour to understanding why
PMS never refreshes a guide incrementally (below, "What we know about PMS's
loads"); this design is what we would build if PMS's own loader cannot be
made cheap. The daily reload (`Converger.refreshCloudGuide`, 4b628b6) stays
the stopgap meanwhile.

## Intent

A Plex cloud lineup's guide (clustarr's `spec.plexGuide`, DIRECTV Mobile on
talos) is populated by cluster-plex's manager from Plex's EPG service, so
that PMS never runs a guide load after the DVR exists: a whole rebuild takes
a minute or two instead of 37, a daily update writes only the new day and
the new programmes, a channel-map change writes only the new channels'
airings, and no PMS thread spends half an hour at a core.

## What we know about PMS's loads (measured 2026-11-04)

- PMS 1.43.5 rebuilds a DVR's whole guide on a channel-map save and on
  `reloadGuide`: every mapped channel, 14 days, two 7-day steps. The
  activity reads "Initializing" from 0% to about 55%, then "Downloaded
  44,433 airings" (the second step's count) to 100%. 21 min for 182
  channels, 29-30 for 233, 37 for 245, 52 for 248 with debug logging on
  for a minute of it.
- The time is one request-handler thread at 86% of a core plus a pool
  thread at 19%: after each step's bulk airings download, PMS fetches
  every programme's metadata one request at a time (`GET
  https://epg.provider.plex.tv/library/metadata/<id>`, 196 serial calls
  in a 50 s window at 60%, each under 100 ms, the gaps spent parsing and
  inserting). Nothing is remembered between loads.
- PMS's nightly refresh is the butler task `RefreshEpgGuides`
  (`ButlerTaskRefreshEpgGuides`, DVR setting "Perform refresh of program
  guide data", about 2 am in the ButlerStartHour-ButlerEndHour window),
  which `prefs.DisabledButlerTasks` turns off with every other task. So on
  this cluster no refresh ever ran; the guide moved only with map saves.
- The hidden preference `DvrIncrementalEpgLoader` (false on this server;
  WebTools-NG lists its default as false; a 2018 Plex forum thread says
  setting it to 0 "causes a complete load", implying incremental was the
  default then) did not change a forced `reloadGuide` at 14:08Z: a whole
  load again. Whether it changes the butler refresh is the open question
  the investigation answers first.

## What PMS writes (the schema `epg_<dvr uuid>` in PostgreSQL)

A guide is an ordinary Plex library schema, cloned from the library's
(`plexbootstrap.CloneSchema`), which the shim serves to PMS. Rows for 248
channels, 14 days:

| table | rows | what |
| --- | --- | --- |
| `media_items` | 85,620 | one per airing: `metadata_item_id`, `library_section_id`, `channel_id` (the DIRECTV number as an integer), `begins_at`/`ends_at` (Unix s), `width`/`height` (1280x720 or 720x480), `duration` (ms), `audio_channels` 2, `display_offset` 0, `media_analysis_version` 0, `extra_data` JSON with `at:channelCallSign`, `at:channelIdentifier` (`<lineup>-<gridKey>`), `at:channelThumb`, `at:channelTitle` ("003 WEAR (ABC)"), `at:channelVcn`, `at:gridKey`, `at:protocol` "livetv" and a `url` form-encoding of the same |
| `media_parts` | 85,620 | one per airing: `id`, `media_item_id` only |
| `metadata_items` | 72,044 | 3,259 movies (type 1), 4,965 shows (2), 7,802 seasons (3), 56,016 episodes (4), 2 playlists (15: `fav://2` and `rec://2`, "Recommendations"); `guid` `plex://show/<id>`, `plex://season/<show id>/<n>`, `plex://episode/<id>`, `plex://movie/<id>`; `hash` = SHA-1 of the guid; `library_section_id` 1 Movies, 2 Shows, 3 Sports; `title`, `title_sort`, `index`, `year`, `duration` (ms), `content_rating`, `tags_genre` "A\|B", `user_thumb_url`/`user_art_url` (metadata-static.plex.tv), `originally_available_at`, `added_at` (the service's), `created_at` (the load's), `changed_at`/`resources_changed_at` (PMS's change counters), `media_item_count`; every `summary` filled; `extra_data`, `user_fields`, `studio`, `rating`, `tags_star` empty |
| `taggings` | 236,346 | item to tag, with `index` |
| `tags` | 1,168 | `tag_type` 1 genres (182), 41 show-level numbers (26; e.g. The Simpsons 11449, Seinfeld 8806: a ranking or external id, unresolved), 309 subjects or teams (712; "AC Milan" to "Zimbabwe"), 310 channels (248, "003 WEAR (ABC)") |
| `library_sections` | 3 | Movies (type 1), Shows (2), Sports (2), each with a uuid |
| `play_queue_generators` | 100 | playlist 72043's items: PMS's recommendations, computed at a load's end |
| `preferences`, `accounts`, `metadata_item_settings`, `metadata_item_views`, `metadata_item_accounts` | 1-2 each | the clone's and PMS's bookkeeping (`fav://2`, `rec://2` with `pv:bulkPlayed`) |
| `schema_migrations`, `sqlite_column_types` | 443, 524 | come with the clone |

No FTS table: the shim answers PMS's `fts4_metadata_titles_icu MATCH` on a
PostgreSQL-backed guide from the titles themselves.

## Inputs: Plex's EPG service (`https://epg.provider.plex.tv`, X-Plex-Token)

- `/` lists the provider's features: `metadata` at `/library/metadata`,
  `grid` at `/grid` (with promoted and genre channel filters), `hero`,
  `lineups`, `promoted` (`/hubs/promoted`), `timeline`, `settings`.
- `/lineups/<id>` and `/lineups/<id>/channels` (at most 1,000 stations,
  no paging): the lineup and its stations (`gridKey`, `callSign`,
  `title`, `vcn`, `isHd`, `hidden`), already fetched by clustarr's tuner.
- `/library/metadata/<id>`: one programme's metadata, what PMS fetches
  per item (a show's `/children` answered empty).
- `/grid?channelGridKey=<k>&date=...`: the airings, by channel grid key
  and a date whose format is not yet known ("Invalid value provided for
  date"); the bulk download PMS does at a step's start was not captured
  (the debug window fell in the metadata phase). Capture it at the start
  of the next load with `logDebug=1`.

## Design (sketch)

1. **Fetch.** For each block on a cloud lineup: the lineup's stations (the
   tuner's `plexmap.json` names the channels and grid keys), the airings
   for 14 days from `/grid` per channel, in parallel under a small limit,
   and each programme's metadata from `/library/metadata/<id>`, through a
   cache in cluster-plex's own PostgreSQL schema (id, JSON, fetched at;
   programmes change rarely), so a daily update fetches only new ids.
2. **Write.** Into the DVR's staging schema (`PrepareStaging`'s clone of
   the library schema, as the copy uses): `library_sections`,
   `metadata_items` in the show, season, episode and movie shapes above
   with PMS's guid, hash, section and tag conventions, `media_items` and
   `media_parts` per airing with the `extra_data` channel fields, `tags`
   and `taggings`; then `analyzeSchema` and `swapGuide`, which PMS reads
   at once. `play_queue_generators` (recommendations) left empty at first
   and watched for what PMS does without them.
3. **Triggers.** Daily in the quiet window (replacing `refreshCloudGuide`'s
   reload); on a channel-map change (cancel the load PMS starts on the
   save, `DELETE /activities/<uuid>`, then write the new channels); on DVR
   creation (let PMS's first load run, or cancel it and write at once);
   `status` and Events on the IPTVProvider as the converger records them.
4. **Oracle.** PMS has built the same guide from the same inputs: a
   comparison of our staging schema with PMS's live one, table by table
   (counts, then rows keyed by guid and by airing), is the development
   test and stays as a recorded-fixture test. Fidelity is the whole risk:
   the grabber matches recordings by these rows, and What's On hubs read
   them.

## Plan (tasks, not yet written as a plan document)

1. Capture a whole load's requests with debug logging on: the `/grid`
   call's parameters, the metadata calls, anything at the end (the
   recommendations).
2. A `pkg/plex/epg` client: lineups, grid, metadata, with the cache.
3. A `pkg/plex/guide` writer: the row shapes, written against the
   staging schema, with the oracle comparison as its test.
4. The manager's loader: triggers, cancelling PMS's own loads, Events.
5. clustarr's side: nothing, unless the map budget or the device id
   rules move.

Estimate: a few days, most of it the writer's fidelity.
