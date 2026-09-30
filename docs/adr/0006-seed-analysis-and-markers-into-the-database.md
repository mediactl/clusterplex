# ADR-0006: Seed media analysis and skip markers into Plex's database

**Status:** Accepted
**Date:** 2026-09-30
**Deciders:** cluster-plex maintainers, with the owner

## Context

On kind-cluster-plex, Plex's own media analysis had stalled:

- Only 64 of 834 movies and 453 of 11,152 episodes had `media_streams`.
- Every other item showed Video/Audio "None" and failed to play with `s1001`.
- Plex had detected credits on 207 items and intros on none.

clustarr already probes every file (`MediaFile.status.mediaInfo`, 12,449 of 13,308 files). TheIntroDB publishes intro, recap, credits and preview segments per title and release. The owner asked for both to reach Plex without its scanner.

**Plex's API cannot write either one.** This was established live against PMS 1.43.4 on 2026-09-30; the evidence is in clustarr's `docs/superpowers/specs/2026-09-30-plex-analyze-bypass-design.md` §2.

- **Media analysis:**
  - The published API (207 paths) has no operation that sets `media_items` or `media_streams`.
  - `PUT /library/metadata/{id}` was tried with seven media and marker variants. Each returned 200 and changed nothing.
- **Markers:** `POST /library/metadata/{id}/marker` creates only `bookmark` markers, and refuses every other type.
- **The provider protocol:** it carries no streams ("Metadata support for 'streams' is not yet supported").

## Decision

The Lease holder's clustarr watch writes clustarr's probe and TheIntroDB's markers straight into Plex's PostgreSQL library, through `pkg/plexseed`. This is an exception to the rule that Plex is changed only through its API. It is bounded:

- **Only two things are written:** media analysis (`media_items`, `media_parts`, `media_streams`) and skip markers (`taggings` rows on the marker tag, `tag_type = 12`). Configuration (providers, agents, libraries, preferences) stays API-only.
- **Only the provisioned libraries** (`provision.Config.LibraryNames()`) are touched.
- **A file Plex analysed is never touched:** any stream Plex wrote means the file is skipped. Plex's own analysis replaces seeded rows whenever it runs, because `media_analysis_version` stays 0.
- **A file replaced in place is seeded again.** squasharr and Tdarr rename an encode over its source, so the path stays and the probe changes. A seeded media item records its probe in `cp:probeHash`. When every stream is the seeder's and the hash differs, the seeder deletes its own streams and seeds them again.
- **A file Plex hasn't scanned yet, or a failed seed, is retried** after 1, 5 and 30 minutes, and after that by the 6-hour resync. Each seed is cut off after 30 seconds, because it runs on the watcher's tick, which also places scans.
- **Seeded rows are identifiable:**
  - media rows carry `cp:source=clustarr` in `extra_data`;
  - markers carry `pv:source=theintrodb`.
- **Markers are reconciled per kind:**
  - If TheIntroDB has segments for a kind, it owns that kind, and Plex's detected rows of it are replaced.
  - If it has none, only the seeder's rows go, and Plex's stay.
  - A failed fetch changes nothing.
- **Every file is one transaction.** Seeding is idempotent, and a replay writes nothing new.

## Consequences

- Playback and skip buttons no longer depend on Plex's scanner reaching every file. That matters because the shared `.LocalAdminToken` makes the scanner fail on every pod but one.
- `pkg/plexseed` depends on Plex's schema:
  - its PostgreSQL tests load `hack/plex-postgresql/schema/plex_schema.sql` (set `CLUSTERPLEX_TEST_POSTGRES_DSN`);
  - a column Plex renames fails the seed loudly, never silently.
- Plex has no recap or preview marker. A recap is written as a second intro, and a preview as non-final credits.
