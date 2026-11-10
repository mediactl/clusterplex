# One guide writer, and the cloud guide built by the managers

**Status: design of 2026-11-10** (owner: "Design and implement both plans",
after "Is there any possible way to intercept this task like we do many
others and distribute the load among the pods?"). Two plans, built in
order: A makes one PMS the only writer of a guide, B has the managers
build a Plex cloud guide themselves, its fetching spread across the pods,
with A's gate keeping PMS's own writes off it. Recording ownership, the
third thing several Live TV pods need, is not part of this design, and
`liveTVNeedsOnePod` stays until it is built.

## The problem

Every PMS writes the Live TV guides on its own (the kind rig, 2026-11-09):

- it refreshes a Plex cloud guide at its start when the guide reads stale,
  and on a timer 24 hours after its last refresh, which no setting turns
  off (`ButlerTaskRefreshEpgGuides` governs only the butler's run);
- it purges airings that have ended, on each guide's own connection.

The guides live once, in PostgreSQL (`epg_<dvr uuid>`), so every pod's PMS
writes the one guide. A refresh is not atomic: PMS empties a two-day
chunk's window per channel and refills it, and deletes what it did not
match at the step's end, so two refreshes at once leave holes or doubled
airings, and one cancelled midway leaves holes (2026-11-04, 165 of 248
channels without two days of airings).

PMS runs the whole thing in its own process (the `EPG[cloud]` threads,
`MediaProviderEpgCloud` compiled into the binary): no helper binary is
started, so cluster-plex's `cmd/shim` never sees it. What does see it is
the PostgreSQL shim, loaded into PMS: every statement a refresh makes
passes through it. Both plans use that.

## Plan A: one PMS writes each guide

### The shim's gate (plex-postgresql)

`PLEX_PG_GUIDE_WRITER_FILE` names a file the manager keeps. When the
variable is set, a write on a **guide connection** (a guide's own file,
`epg_schema_for_path`, not a load file) runs only if the file allows the
guide's schema. Each line of the file is one of:

| line | means |
| --- | --- |
| `*` | every guide may be written |
| `epg_<uuid>` | this guide may be written |
| `-epg_<uuid>` | this guide may not, whatever else the file says |

An absent or empty file allows nothing. The file is read again when its
modification time or size changes, looked at no more than once a second.
The variable unset is today's behaviour: every write runs.

A write the file does not allow is **held**, not refused: it is not sent
to PostgreSQL, and PMS is told it ran.

- `sqlite3_step` answers `SQLITE_DONE`, `sqlite3_changes` 0;
- `sqlite3_last_insert_rowid` answers a synthetic id, unique in the
  process (counting down from 2^62), so PMS's in-memory maps keep distinct
  keys for rows it believes it inserted; a later read or write by such an
  id matches nothing;
- a statement that returns rows (a `RETURNING` of PMS's own) answers none.

Refusing instead would fail PMS's loader: a refused statement killed it
once already (the statement timeout of 2026-11-04), left a ghost "Refreshing
guide data" activity until PMS restarted, and made it retry every five
minutes.

What counts as a write is what the shim's write path already executes:
`INSERT`, `UPDATE`, `DELETE`, `REPLACE` and DDL. Transaction control
(`BEGIN`, `COMMIT`, `ROLLBACK`, `SAVEPOINT`, `RELEASE`) passes, so PMS's
transactions open and close as they did. Reads pass.

Each schema's first held write after it was allowed (and its first allowed
write after it was held) logs once at INFO; a counter of held writes per
schema is in the shim's stats.

Loads are not gated in the shim: a load writes its own file, or, with
guide loads in PostgreSQL, its staging schema through the manager's stage
and swap. The manager gates those.

### The manager (cluster-plex)

- `shimEnv` sets `PLEX_PG_GUIDE_WRITER_FILE` to
  `<Databases>/.clusterplex-guide-writer` on the pod's own `emptyDir`, and
  the manager writes it empty before PMS starts.
- While the pod holds the plex.tv Lease, the file reads `*`, plus a
  `-epg_<uuid>` line for every guide the managers build (plan B). Losing
  the Lease empties it.
- Only the holder copies a guide load in (`copyGuideLoads`), stages one or
  swaps one (the guide socket answers `skip` elsewhere). A load a pod
  without the Lease makes is left in its own file: its PMS reopens the
  guide by name, which the shim serves from the shared schema.

With one Plex pod the holder writes everything, as now.

## Plan B: the managers build a cloud guide

### Inputs

Plex's guide service, `https://epg.provider.plex.tv`, with the server's
token (`X-Plex-Token`; PMS sends the same value as `X-Plex-Api-Token` to
`/library/metadata`), the calls PMS makes (its debug log, 2026-11-09):

- `GET /lineups/<lineup>/channels`: the stations, each with its `gridKey`;
- `GET /grid/bulk?channelGridKey=<k>&startDate=<day>&endDate=<day+1>`:
  one channel's airings for one day, each a `Metadata` (an episode or a
  movie, with `grandparentGuid`, `parentIndex`, `Genre`, `Image`) holding
  a `Media` (`beginsAt`, `endsAt`, `premiere`, `live`, `videoResolution`);
- `GET /library/metadata/<ratingKey>`: an item's own metadata, where the
  grid lacks what PMS writes (which fields, the oracle decides).

### The cache

A table in cluster-plex's own schema of the library database,
`clusterplex.epg_cache (key text primary key, fetched_at timestamptz,
body jsonb)`, keys `grid/<gridKey>/<YYYY-MM-DD>` and `metadata/<ratingKey>`.
Every pod's manager writes it and the holder reads it.

### Fetching, spread across the pods

Each serving pod's manager keeps its share of the channels' days fresh for
today and the next 14 days: a channel belongs to the pod the consistent
hash (`pkg/hashring`, as the maintenance fan-out uses) gives its grid key
among the serving pods. A day is fetched again when its entry is older
than 6 hours (today and tomorrow) or 24 hours (later days); an item's
metadata once a week. Fetches run four at a time per pod. The holder
fetches whatever is still missing when it builds.

With one pod, the holder fetches everything; the time is network-bound
and parallel, minutes where PMS's serial load took 8.5 (2026-11-04: "network:
512.6 sec").

### Building

The holder, for a block on a cloud lineup whose DVR the managers build:

1. `PrepareStaging` (a clone of the library schema's structure);
2. copies `library_sections` and the two playlists PMS keeps
   (`fav://`, `rec://`, metadata type 15) from the live guide, so section
   ids and uuids stay what PMS knows;
3. writes, from the cache, for every channel of the block's map and every
   day: a show (type 2), a season (3) and an episode (4), or a movie (1),
   once per guid, `hash` = SHA-1 of the guid (verified against PMS's
   rows); a `media_items` row per airing with the channel's `extra_data`
   (`at:channelCallSign`, `at:channelIdentifier`, `at:channelThumb`,
   `at:channelTitle`, `at:channelVcn`, `at:gridKey`, `at:premiere`,
   `at:protocol`, and the `url` form PMS keeps beside them) and a
   `media_parts` row; genres and the other tags as `tags` and `taggings`;
4. `analyzeSchema`, `swapGuide`, which PMS reads at once;
5. writes the provider row as PMS does after a refresh
   (`pv:timeOfLastRefresh` now, `pv:lastChunkEndedAt` the build's last
   day), so PMS's start-up check finds the guide fresh.

Every column follows what PMS writes, field by field, checked by the
oracle below.

### PMS's own writes

The built guide's `-epg_<uuid>` line in every holder's writer file holds
PMS's writes to it on every pod: its refreshes and its purges. A purge is
not needed: each build writes only from now on.

A channel-map save makes PMS start a whole load (into its load file, not
the guide). The converger cancels that load's activity (a load leaves the
live guide alone, so a cancel costs nothing there) and asks for a build.

### Triggers

- daily, in `plex.liveTV.guideRefreshWindow`, replacing the whole reload
  `refreshCloudGuide` asks PMS for;
- when the block's channel map changes;
- at the manager's start when the guide is older than a day or empty.

Never during a recording or a Live TV session on the holder, as now.

### The oracle

PMS has built the same guide from the same inputs. `hack/guidediff`
compares a built staging schema with PMS's live guide table by table;
the development loop runs on the kind rig's cloud DVR (12 DIRECTV
channels), and a recorded fixture (the rig's grid and metadata answers,
and the rows PMS wrote from them) keeps it as a test.

### Configuration

`plex.liveTV.cloudGuide`: `pms` (the default: PMS loads and refreshes its
cloud guides) or `manager` (the managers build them). Plan A's gate is on
whenever Live TV is.

## Order

1. A in the shim, with tests; a release.
2. A in the manager; the rig with two pods: a pod without the Lease
   refreshes, and the guide is untouched.
3. B's client and cache; the fetch loop and its sharding.
4. B's writer, to an empty oracle diff on the rig.
5. B's triggers and the converger's cancel; the rig end to end.

## As built (2026-11-10)

### Plan A

- The shim's gate is plex-postgresql `v1.3.17-clusterplex.54`
  (`guide_writer.rs`), on the prepared, cached and `sqlite3_exec` write
  paths; the file is re-read on a change of its modification time, size or
  inode. `.55` changed what a held write answers (below), and
  cluster-plex's Dockerfile takes `.55`.
- **A held write must answer as a write that ran.** `.54` answered
  `changes()` 0 and row ids counting down from 2^62. On the rig PMS's
  incremental refresh of the built guide died at its first held write, an
  `INSERT INTO tags`, the moment it read the row id back ("Uncaught
  exception running async task ... std::exception"); 2^62's low 32 bits are
  zero, so a PMS that keeps the id in a 32-bit integer read 0. The refresh's
  "Refreshing guide data" activity then stayed at 0% until PMS restarted,
  which would have held the converger (it waits out a guide activity) and a
  new holder's claim (`keepGuideWriter`) for good. `.55` answers an
  INSERT's or REPLACE's `changes()` with the rows it lists, an UPDATE's with
  one and a DELETE's with none (`held_changes`), and counts row ids down
  from 2^31-1. The same refresh then ran its seven steps in 197 s, and the
  guide's rows were unchanged.
- A new holder waits at most 45 minutes for its PMS's guide activity
  (`guideWriterWaitMax`) before it takes the guides, so a refresh that dies
  some other way cannot keep them from their writer.
- The manager's side is `cmd/manager/guidewriter.go`. The gate is on when
  `LiveTVConfig.GuideWriter()` is: Live TV with `plex.liveTV.severalPods`,
  or with `plex.liveTV.cloudGuide: manager`. With one pod and PMS's own
  cloud guides it is off, and the shim writes as it always did.
- The election's first attempt now runs before PMS starts, and PMS's start
  waits up to 10 s for it (`prepareGuideWriter`), so the file PMS starts
  with says what the Lease says. A pod that takes the Lease while its PMS
  loads or refreshes a guide waits for that to end before it writes `*`
  (`keepGuideWriter`): a refresh whose first writes were held and whose last
  were not would delete airings it never inserted. Losing the Lease empties
  the file at once.
- Only the holder stages, swaps or copies a load (`loadsNotOurs`), and a
  start's copy-in and sweep of staging schemas run only on the holder
  (`PrepareGuides`' writer argument): a pod without the Lease once swept
  the staging schema the holder was loading into.
- `plex.liveTV.severalPods` lifts `liveTVNeedsOnePod` and the chart's
  guard, for a deployment that records nothing: recordings still have no
  owner (every PMS records every rule).

### Plan B

- `pkg/plex/epgcloud` is the guide service's client and the cache table
  (`clusterplex.epg_cache`); `pkg/plex/guidebuild` plans and writes the
  rows; `plexbootstrap.InstallBuilt` clones a clean staging schema, carries
  the live guide's sections, playlists (renumbered past the build's items)
  and viewing rows over, writes the build with the tables' triggers held
  off, and swaps; `plexbootstrap.MarkGuideRefreshed` writes the provider
  row; `cmd/manager/cloudguide.go` is the fetcher (every pod) and the
  builder (the holder); the converger calls it through
  `plexlivetv.CloudGuideBuilder`.
- The cache keeps each answer's bytes as fetched (`body bytea`, not
  `jsonb`), under `lineup/<id>`, `grid/<gridKey>/<from>/<to>` (one week per
  call, as PMS asks) and `metadata/<ratingKey>`, and the holder's plan for
  a build under `plan/<dvr uuid>`; the fetchers are paced by rate, not run
  four at a time.
- **The service limits each token, and every pod shares one.** On the rig
  PMS's own load made 496 metadata calls in 75 s without a refusal; 250
  more, eight at a time, were half refused (429, "Rate limit exceeded!")
  until some minutes later. So spreading the fetching across pods divides
  one budget: `cloudGuideRate` (3 a second) is every pod's together, each
  pod taking its share, and the cache keeps a show's metadata a week, so a
  daily build fetches the weeks (two calls a channel) and the new shows.
  The first build of a 248-channel lineup is about 500 grid calls and some
  thousands of metadata calls, the better part of an hour at that rate, as
  PMS's own first load is; later ones are minutes. The build itself, from
  the cache, took 1.5 s on the rig against PMS's 76 s load.

### The rig, end to end (2026-11-10)

The kind rig's 12-channel DIRECTV DVR, under `cloudGuide: manager`, then
`severalPods: true` with two pods:

- **The build.** The converger adopted the DVR, saved its map from the fake
  tuner's `plexmap.json` and cancelled the load PMS started for it. The
  builder planned the guide, fetched 24 week-grids and about 500 items'
  metadata at the shared 3 a second with no refusal, and installed 12
  channels, 4,003 items and 3,777 airings, 204 s in all, of which the
  install took 6.6 s. PMS's own grid API then listed every channel's
  airings with the channel titles PMS writes.
- **PMS's own writes, held.** With the provider row made stale, PMS's boot
  refresh of the built guide on the holder ran all seven steps (197 s,
  under `.55`) and on a second pod (58 s), and the guide's rows, counted
  and hashed, were the build's after both.
- **The writer file.** Each pod's file was empty before its first turn at
  the Lease. The holder's read `*` and the built guide's hold; the second
  pod's stayed empty. At failover the old holder emptied its file as it
  gave the Lease up, and the second pod wrote `*` 14 s later. The
  replacement pod started empty.
- **Found on the way.** The load watch still made a staging schema for a
  load this pod does not copy, which for a built guide could race the
  build's clone; `stageGuide` now refuses such a load. And `.54`'s held
  answers, above.

### The rig again, with the exact build (2026-11-10, shim .57)

Each step on the same 12-channel DVR, checked by the oracle on the guide the
manager installed: the manager's cache exported as a fixture, built by
`hack/guidebuild` at the install's own time, compared in all five tables,
ids included (identical every time).

- **A first build** (9 s, the metadata already cached), served by PMS: its
  grid API listed exactly the airings the schema holds, on every channel,
  and the What's On hubs filled. Sequences end past the build's ids,
  playlists sit past its items, sections and viewing rows came over.
- **PMS's boot refresh held**, single pod and holder: the provider row made
  stale and PMS restarted, its incremental refresh ran seven steps in 42 s
  and every guide table's rows hashed alike before and after.
- **A restart builds nothing; a day-old guide waits for
  `guideRefreshWindow`** (8-11 UTC by default) and **a two-day-old one is
  built at once** (10 s).
- **A map change** (one channel dropped, the lineup hash moved): the
  converger saved the 11-channel map, cancelled PMS's load in the same
  second, and the build installed 11 channels 12 s later; DVR, activities
  and the other channels untouched.
- **Two pods** (`severalPods`): the holder's writer file read `*
  -epg_<uuid>`, the other's was empty; a shared rebuild from a cache made
  wholly stale (520 calls) took 266 s, the other pod fetching its share
  (the holder built before its own five-minute deadline) and the holder
  building 15 s after the last metadata arrived; failover handed the
  writer file over in 14 s and built nothing; the non-holder's boot
  refresh ran to its end and changed no row.

Fixed on the way:

- **The holder waits for metadata, not grids alone** (`cloudGuides.missing`,
  `epgcloud.CacheStale`): a pod fetches its channels' grids before their
  shows and movies, so with several pods the holder could build while
  another still fetched, leaving those shows without summary or genres.
- **A mapped channel the lineup lacks is left out** (`Stats.Unmatched`,
  logged), as PMS's load adds only the channels it matches; it had failed
  every build, so one station gone stopped the whole guide.
- **A 500, 502, 503 or 504, or no answer, is asked again** twice (2 s, 5 s)
  before a build fails (`epgcloud.retryBackoff`): PMS's own refresh met a
  503 mid-load and went on; one would have failed the build, which keeps
  the old guide and tries again in 15 minutes.
- **The provider row's `updated_at`** is the build's time too: PMS reports
  it as the DVR's `refreshedAt`, and its own refresh writes the two alike.
  PMS keeps the row it read at its start and writes it back after any
  refresh of its own, so its API shows a build's time only after a restart;
  the row is PMS's, and the build's write only spares PMS a needless (and
  held) boot refresh.

### What PMS writes (the rules the oracle settled)

The rig's full load of 12 DIRECTV channels (3,787 airings, 4,004 items)
against a build from the same answers, fetched in the same minutes:

- PMS reads every channel's first week, in lineup order, before any second
  week, and that order decides each item's "first" airing. A channel is the
  lineup's station with both the map's grid key and its number (the map's
  `lineupIdentifier`): the lineup lists one station under several numbers
  (DTV203 at 002, 078 and 203; 64 of DIRECTV Mobile's 1,000), each a place
  of its own, and the matched place is the channel's turn and tag id. The
  12-channel rig never showed it; the map's order is not the load's.
- An airing that ended before PMS read its answer is not written, nor an
  item with nothing still to come; a slot both weeks' answers hold is
  written once. A long load reads its channels minutes apart, so the
  oracle gives each channel's week the moment PMS asked for it
  (`Input.ReadAt`, from the capture's log); a build reads all at once.
- Every airing of an item carries the item's duration, which is the slot
  length (end minus start) of its first airing; a movie's or episode's
  duration is the same.
- A channel tag's id is its station's place in the lineup's list, from 1,
  and an airing's `channel_id` is that id; genres and teams follow from the
  list's length. Its title is number, call sign and the station's
  affiliate network, else its title ("003 WEARDT (ABC)"), the network left
  off when it is the call sign ("244 SYFY"); its key is empty, its
  `tag_value` 1 for an HD station and NULL otherwise. An airing of a
  station without a logo has no `at:channelThumb`. An airing's resolution
  is 720x480 at 480 and 1280x720 above (720 and 2160 alike).
- `extra_data` is compact JSON in key order with a `url` copy in PMS's
  escaping, which escapes `.` too; a premiere airing carries
  `at:premiere` and `media_analysis_version` 1.
- A show is known by its section and title: one "College Football" under
  two ids is one show (the season keeps its episode's own show id), but
  "College Football Scoreboard" in Sports and in Shows is two. A show is in
  Sports when its own metadata genres, or its first episode's, name the
  Sports genre ("Sports talk" does not), or the episode names teams.
- A season's year and a show's content rating are its first episode's.
- A numbered episode joins its own season (its show id and index). One
  without a number joins the show's season of its index, else the season
  of its year (the index when that is a year, else the episode's own
  year): the show's season of that index, else the first made numbered
  season of that year, of any of the show's ids; failing those it makes
  its own (39,108 of 39,108 at 248 channels; "the most recently made
  numbered season", the 12-channel rule, was a coincidence there).
- `tags_genre` is an item's first two genres; its taggings carry them all,
  and a channel's tagging index is its place among the item's channels.
- Sort titles fold accents (not a double acute: "Gergő"), drop leading
  quotes, brackets and dots but keep "#", "$" and "&", and drop a leading
  "The", "A", "An", "El", "La", "Der" or "Das" in any case, never "Los",
  "Las", "Le", "Les", "Die", "Il" or "O" (48,548 of 48,548).
- A date with fractional seconds is not kept; a show or a movie without a
  kept date is dated its year's first day, an episode not.
- What PMS leaves unset is an empty string, never NULL (but `trivia`,
  `quotes`, `extra_data`); `is_adult` is 0; `index` -1 for an episode
  without a number.

- `media_item_count`: an episode's is the airings listed by the answer it
  was made from (ended and repeated ones too), plus every airing written
  for it, less one; a movie's is the airings written for it, less one
  (3,018 of 3,018 episodes, 155 of 155 movies). Counting every slot the
  answers list missed 262 episodes and 4 movies.
- A movie's genres are its grid answer's and its own metadata's lists
  joined, sorted by name with libc++'s `std::sort` as it was until LLVM 15
  (unstable: insertion sort only up to 6 elements of a type that is not
  trivially copyable), and the first of each name kept, at its place in
  the joined list (`guidebuild.mergeTags`, `libcxxSort`, 155 of 155;
  modern libc++, a stable sort or any other key matched 54 or fewer). Where
  the two lists agree, as they do on the rig, each genre is kept at one of
  its two places, so the indexes skip and the order (and `tags_genre`, the
  first two) moves: Action, Adventure, Fantasy, Movies, Adrenaline + Sci-Fi
  become Action 0, Movies 3, Adventure 6, Fantasy 7, Adrenaline + Sci-Fi 9.
  Identical lists always end alike (21 groups of 21).
- Ids: PMS makes a new episode's show and season before the episode, so
  ids run show, season, episode. It writes an item's taggings as it meets
  it -- genres, teams, then its first channel -- and when the item turns
  up on another channel it deletes the item's channel taggings and writes
  them all again, so those ids follow that moment and the deleted ones are
  never used (90 gaps on the rig). An airing part's `file`, `hash` and
  `open_subtitle_hash` are empty strings.
- `added_at` of an item the service gives no `addedAt` is PMS's own clock
  as it writes the item: the second the answer that brought it arrived (a
  show's or movie's metadata, an episode's grid page), plus the
  milliseconds PMS took (87% to that second, the rest the next). No build
  can reproduce it and PMS would not repeat it, so the oracle reads two such
  stamps near the load as the load's time, as it does `created_at`.

**The oracle, exact (2026-11-10).** With those rules a build matches a
load PMS made, every row and column of `metadata_items`, `media_items`,
`media_parts`, `tags` and `taggings`, ids included (`hack/guidebuild`
compares ids since). Twice:

- The first load (03:16Z, 4,004 items, 3,787 airings), on whose answers the
  rules were found: one row differs, a sports event's summary, which PMS
  wrote empty and the fixture, fetched two minutes after PMS's load, gives
  as its venue.
- A second load of the same DVR (09:56Z, 3,942 items, 3,708 airings), its
  fixture captured while PMS loaded: each grid week fetched the moment
  PMS's log showed PMS asking for it, the metadata after. No row differs;
  101 movies kept genres at their second places and 254 episodes counted
  more than their written airings, and the same sports event's summary was
  the venue in PMS's load and the build alike: the first load's difference
  was the service changing between two fetches, not a rule.

The recorded answers are Plex's guide data and stay out of this public
repository. `hack/guidebuild` builds from a fixture directory recorded on
the rig (PMS's debug log names every request of a load; fetch the same
URLs, and freeze PMS's result as the reference schema) and compares column
by column, `-now` the load's start; `pkg/plex/guidebuild`'s tests hold each
rule on synthetic answers. Fetch each grid week the moment PMS's log shows
it asked (a debug container tailing the log, its output followed with
`kubectl logs -f`), since the service changes between fetches minutes
apart, and the metadata after the load at no more than 3 a second, which
keeps PMS's own requests under the token's limit. Send the server token as
`X-Plex-Token`: the `X-Plex-Api-Token` in PMS's requests is its own and the
service refuses another caller's (401).

### At scale: 248 channels (2026-11-10, shim .57)

PMS's own load of the rig's DVR on 248 DIRECTV Mobile channels (talos's
number), captured as before: 1,427 s (two steps, 564 s and 1,353 s, 0
HTTP errors), 48,548 items, 70,912 airings, 184,226 taggings, 798 tags;
the manager's copy into PostgreSQL 375,854 rows in 73 s. The first
comparison differed in channel order, seasons, sort titles, a channel
tag's title and flag, an airing's resolution and logo, and a movie's date
-- rules twelve channels never exercised, each now above and held by a
test that fails without it. The build then matched the load in every row,
column and id of all five tables; only `added_at` from PMS's own clock
differs, and the tool reads those as the load's time across the whole
captured load. The build from the captured answers took 41 s.

Then through the managers themselves (cluster-plex 29e54da, two pods,
`severalPods`, the cache emptied first, the build in the holder's process):
from the plan to the install 35.7 minutes, 4,935 calls at the shared 3 a
second with three one-minute pauses for the service's 429s, the install
itself 45 s; the oracle on the installed guide exact in all five tables
(48,267 items, 70,480 airings, 183,251 taggings), and PMS's grid API
listing exactly the schema's airings for the hour ahead. **The holder's
manager peaked at 603 MB** (VmHWM, from about 80 MB; the pod's cgroup at
652 MB) while it built, the other pod's at 79 MB: a guide's rows are held
whole before they are written, which is the cost the build Job below takes
off the Plex pod.

### The build in a Job, and one budget (2026-11-10, owner)

Owner, 2026-11-10, on learning the build ran in the Plex container beside
PMS: "Is there any way to distribute the compute or offload it to a job
reliably?", then "Yes" to: the fetch stays with the pods, the build moves
to a Job, the swap stays with the holder, and the pods' per-pod 3/N pacing
becomes one budget in PostgreSQL.

- **Why only the build moves.** The fetch is bound by the token's rate, not
  by compute: more pods or Jobs divide the same 3 calls a second. The swap
  must be the holder's, which knows when PMS loads. The build is CPU and
  memory over the cache and writes one staging schema, and needs neither
  PMS nor the token. It is one global pass, since ids follow PMS's order
  across every channel (the rules below), so it is one Job, not shards.
- **The split.** `plexbootstrap.InstallBuilt` became `StageBuilt` (the
  staging schema cloned clean, the live guide's sections, the build, the
  sequences, and a row in `clusterplex.guide_builds` naming the build's
  stamp, all in one transaction, under the guide's advisory lock) and
  `SwapStaged` (only the stamp it is given, else `ErrNotStaged`; the live
  guide's playlists and viewing rows carried at the swap, then the rename).
  `InstallBuilt` is the two in a row, for `cloudGuideBuild: inProcess`.
- **The Job** (`cmd/manager/cloudguidejob.go`): named
  `plex-guide-build-<dvr>-<stamp>`, the stamp a hash of the plan as cached;
  `manager guide-build --dvr --stamp` on the holder's image, as uid 65534,
  read-only root, no ServiceAccount token, the holder's
  `CLUSTERPLEX_POSTGRES_*` environment alone (the password stays a Secret
  reference), GOMEMLIMIT 90% of its memory limit, owned by the StatefulSet,
  labelled `guide-build` so no Plex selector matches it; `backoffLimit` 2,
  `activeDeadlineSeconds` 3600 unless set, kept 6 hours, exit 3 (the cached
  plan is not its stamp) fails it at once through `podFailurePolicy`. The
  holder makes sure the lineup is fresh in the cache first (the Job holds no
  token), then waits for the record of its stamp, polling every 10 s; a
  failed Job is an error, and the builder's next pass (15 minutes) deletes
  it and starts another. A new holder finds the same Job, or the staged
  build, by the stamp.
- **A PMS start's sweep spares a build.** `SweepGuides` dropped every
  `_next` schema at a PMS start ("no load runs at a start"), true while
  only the holder's own process built. A Job builds outside every pod, so
  the sweep now keeps a staging schema whose guide lock is held (a build or
  a copy at work, `pg_try_advisory_lock`) or that `guide_builds` names (a
  build awaiting its swap) (`stagingBusy`). Found in review, before the rig.
- **One budget.** `epgcloud.Budget`: one row per token in
  `clusterplex.epg_budget`; a call claims `greatest(next_at, now) +
  interval` in one statement and sleeps until its slot, the arithmetic on
  the server's clock; a 429 pushes `next_at` past its `Retry-After` for
  every pod. PMS's own calls with the token are still outside it.
- **Tested** against PostgreSQL 15: stage and swap by stamp, a failed stage
  leaving nothing to install, a playlist made during the build carried at
  the swap, the stage waiting on the guide's lock, the start sweep keeping a
  build's staging schema (falsified: fails without `stagingBusy`); two connections sharing
  one rate and one hold; the Job's pod spec; adoption and replacement of a
  Job; the holder waiting for a Job's record, taking one already staged,
  and failing with a failed Job; and `manager guide-build` itself, from the
  environment, staging the stamped plan and refusing a moved one. Not yet
  on the rig: a real Job on kind, its memory at 248 channels, and a holder
  failover mid-build.
