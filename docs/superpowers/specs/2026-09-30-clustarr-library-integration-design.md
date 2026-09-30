# Take the library's metadata from clustarr, with nothing set in the Plex UI

**Date:** 2026-09-30
**Status:** Proposed. The owner chose the recommended options on 2026-09-30:

- Libraries use clustarr's providers only, with no Plex fallback agent.
- cluster-plex watches clustarr's resources rather than clustarr pushing
  notifications.
- An existing library is switched to clustarr only when the operator says so
  per library.

**Companion:** clustarr
`docs/superpowers/specs/2026-09-30-plex-provider-filename-match-design.md`,
which makes the provider match a file by its path instead of by title.

## 1. Goal

A cluster-plex install that points at a clustarr install ends up with:

- clustarr's two custom metadata providers registered;
- an agent for each, holding only that provider;
- a Movies and a TV library using those agents;
- Plex rescanning exactly the folders clustarr changes, and refreshing
  exactly the items whose metadata clustarr changes.

All of it follows from `config.yaml`. Nobody opens Settings → Metadata
Agents, and nobody adds a library by hand.

### Non-goals

- **No writes to Plex's database.** Providers, agents and libraries live in
  `metadata_agent_providers`, `metadata_agent_provider_groups`,
  `metadata_agent_provider_group_items` and `library_sections`. PMS owns
  those tables and caches them, so everything here goes through its admin HTTP
  API. `pkg/plex/db` stays read-only.
- **No writes to clustarr.** The watcher only reads. clustarr does not know
  cluster-plex exists.
- **No deletions.** Removing a library or provider from the config leaves it
  in Plex.
- **Movies and TV only.** Custom providers support nothing else yet.
- **The binary shims do not change.** Custom scanners and legacy agents do not
  work with Plex's modern agents: Plex staff answer "No, and no" to a custom
  scanner under the Plex Series agent
  (forums.plex.tv/t/881511). So the shim remains a way to *place* work — the
  transcoder on workers, and the scanner with the PostgreSQL preload — never a
  way to feed metadata.

## 2. What the live cluster showed

These came from read-only queries on kind-cluster-plex on 2026-09-30.

- PMS is **1.43.4.10903**. The provider endpoints need 1.43.0 or later.
- Both providers are already registered, apparently by hand around
  2026-09-25, and Plex reports both online:
  - id 9, `tv.plex.agents.custom.clustarr.movies`
  - id 10, `tv.plex.agents.custom.clustarr.tv`
  - both at `http://clustarr-ui.clustarr-system.svc.cluster.local:8080/plex/…`
- Groups 7 and 8 hold one provider each.
- No library uses them. The only library is `Movies` (id 1): agent
  `tv.plex.agents.movie`, group 1, folder `/media/Movies`.
- Plex's `/media` is a 1Gi `local-path` claim, **not clustarr's library**.
  clustarr's RootFolders are `/data/media/{movies,tv,books}`.

The provisioner therefore has to adopt existing registrations rather than
create duplicates, and the integration needs the shared volume described in
§3.

## 3. Prerequisite: Plex reads clustarr's library

Plex can only scan what it can see.

- `persistence.media` gains `existingClaim` (and `subPath`). The chart then
  mounts the claim clustarr already uses for `/data` instead of creating
  `<release>-media`.
- The mount path stays `/media`, the same in every pod, as now.
- The difference in prefix between the two sides is what `pathMappings`
  (§4) is for.

Without this, the provisioner still runs, but every watched path fails to
map (§6.2) and is counted as unmappable.

## 4. Configuration

These are new keys under `plex:`, following the `plex.preferences` pattern:
lists, not maps, because viper lowercases map keys. Each is read with
`UnmarshalKey` in `cmd/manager/config.go` and rendered by
`templates/storage.yaml`.

```yaml
plex:
  metadataProviders:
    - uri: http://clustarr-ui.clustarr-system.svc:8080/plex/movies
    - uri: http://clustarr-ui.clustarr-system.svc:8080/plex/tv
  libraries:
    - name: Movies
      type: movie            # movie | show
      provider: tv.plex.agents.custom.clustarr.movies
      language: en-US
      locations: [/media/movies]
      switchAgent: false     # see §5.3
    - name: TV
      type: show
      provider: tv.plex.agents.custom.clustarr.tv
      language: en-US
      locations: [/media/tv]
  clustarr:
    enabled: true
    namespace: clustarr-system
    pathMappings:
      - {clustarr: /data/media, plex: /media}
```

**Validation at startup is fatal**, the same way a refused preference is:

- a library's `provider` must be the identifier of a configured provider (it
  is learned from the provider's root, §5.1, so this check runs at the first
  reconcile, not at parse time);
- `type` must be `movie` or `show`;
- `locations` must be absolute and non-empty;
- `pathMappings` prefixes must be absolute, and no two `clustarr` prefixes may
  be equal.

## 5. The provisioner (`pkg/plex/provision`)

It runs only on the pod holding the Lease. Starting and stopping on the
existing leader transitions (`cmd/manager/elect.go`) gives Plex's
configuration one writer at a time. It calls PMS on the pod's own
`127.0.0.1:32400` with the token `plexToken()` already reads, and it runs:

- once Plex answers `/identity` after the Lease is won;
- then every 10 minutes.

Every step is idempotent, so a second run against a converged server issues
no writes. All endpoints come from the PMS OpenAPI spec (developer.plex.tv,
API 1.2.3) and are admin-only.

### 5.1 Providers

1. `GET /media/providers/metadata`.
2. For each configured `uri`:
   - fetch the provider root and read its `MediaProvider.identifier`;
   - if no registered provider has that identifier, `POST
     /media/providers/metadata?uri=<uri>` (a 409 counts as success);
   - if one has it under a different `uri`, `PUT
     /media/providers/metadata/{id}?uri=<uri>`.
3. If the root is unreachable, or answers 503 (clustarr's provider does until
   its `--external-url` is set), log once, mark the provider not converged,
   and retry with backoff (from 30 s up to the 10-minute resync).

Plex readiness never waits on this.

### 5.2 Agents (provider groups)

1. `GET /media/providers/metadata/group`.
2. For each provider a library names, find the group whose
   `primaryIdentifier` is that identifier.
3. If there is none, `POST
   /media/providers/metadata/group?title=<provider title>&primaryIdentifier=<id>`.
   PMS adds the primary provider as the group's first item itself.
4. A group with other items is left alone. The owner chose clustarr only, so
   the provisioner adds no fallback providers and removes none an operator
   added.

The live groups 7 and 8 are adopted as they are.

### 5.3 Libraries

1. `GET /library/sections` and match each configured library by `name`.
2. **If it is absent**, create it:

   ```
   POST /library/sections?name=&type=movie|show&agent=<provider id>
        &scanner=Plex Movie|Plex TV Series&language=
        &location=<each>&metadataAgentProviderGroupId=<group id>
   ```

   This is the form working third-party code uses (FanKarr, SportScanner).
   The spec's `POST /library/sections/all` form is the fallback if PMS
   refuses it.
3. **If it is present on the configured group**, nothing is done.
4. **If it is present on another group** — the live `Movies` library — it is
   drift:
   - With `switchAgent: false` (the default), log it once per run, set
     `clusterplex_library_agent_drift{library}` to 1, and change nothing.
   - With `switchAgent: true`:

     ```
     PUT /library/sections/{id}?agent=<provider id>
         &metadataAgentProviderGroupId=<group>
     POST /library/sections/{id}/refresh?force=1
     ```

     Always send `agent`: PMS answers 400 "'agent' is missing" without it.
     The forced refresh is needed because a new agent otherwise applies only
     to items added later.
5. Locations of an existing library are not reconciled. A difference is
   logged as drift and left to the operator.

### 5.4 Errors and metrics

Any other non-2xx answer is logged with its status and a capped body, and
retried at the next run.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `clusterplex_provision_runs_total` | `result` = `converged` / `pending` / `error` | Outcome of each run |
| `clusterplex_library_agent_drift` | `library` | 1 while a library is on another agent |

Library names are operator-chosen and few, so they are an acceptable label.
Nothing is labelled by title or path.

## 6. The clustarr watcher (`pkg/clustarrwatch`)

It also runs only on the Lease holder, started and stopped with the
provisioner. It uses **dynamic informers over `unstructured` objects**, and
does not import clustarr's Go module. This keeps GPL-3.0 code out of
cluster-plex's dependency graph, and it pins the watcher to a handful of field
paths, declared as constants and tested against copies of clustarr's example
objects.

| Resource (`catalog.clustarr.io/v1alpha1`) | Fields kept (a transform drops the rest, including `managedFields`) |
| --- | --- |
| `mediafiles` | `metadata.{name,namespace,uid}`, `spec.path`, `spec.mediaRef.kind` |
| `movies`, `series`, `episodes` | `metadata.{name,namespace,uid}`, `status.metadata`, `status.overlay`, and for episodes `status.{title,overview,airDate}` |

The transform matters: the owner's library is about 16,000 items and 14,000
files, and clustarr learned the hard way that full objects make every cache
slow (clustarr's CLAUDE.md, "Every cache strips managedFields").

### 6.1 Startup

The informers' initial list is a **baseline, not a change**. It records each
file's path and each item's metadata hash, and triggers nothing. Otherwise
every Lease change would rescan the whole library.

Changes missed while no pod was watching are caught by one **non-forced**
`POST /library/sections/{id}/refresh` per configured library, issued once the
informers sync. Plex's non-forced refresh scans only folders whose
modification time changed, so this is cheap. The nightly `refresh` CronJob
remains the backstop.

### 6.2 Files → partial scans

On a MediaFile whose `mediaRef.kind` is `movie` or `episode`:

1. **Add, update or delete.** Map `spec.path` through `pathMappings`,
   longest prefix first.
   - If it cannot be mapped, increment
     `clusterplex_clustarr_unmappable_paths_total` and drop it.
2. **A changed path** — a rename, a transcode that changed the container, or
   `renameTranscoded` — enqueues both the old and the new folder.
3. **Pick the library** whose location is a prefix of the mapped path, from a
   `GET /library/sections` result cached for 5 minutes. If none covers it,
   increment `…_uncovered_paths_total` and drop it.
4. **Debounce** per (library, folder) for 30 s, then:

   ```
   POST /library/sections/{id}/refresh?path=<folder>
   ```

5. If more than 50 folders of one library are queued in one window, a single
   non-forced section refresh replaces them. This is autoscan's behaviour, so
   an import burst does not become hundreds of scans.

### 6.3 Metadata → item refreshes

On a Movie, Series or Episode whose hash of the kept fields changed:

1. Build its Plex guid: `<provider identifier>://<movie|show|episode>/<uid>`.
   This is the `{scheme}://{type}/{ratingKey}` form clustarr's provider
   issues, and it uses the clustarr UID as the rating key.
2. Look up Plex's item id with a read-only query:

   ```sql
   SELECT id FROM metadata_items WHERE guid = $1
   ```

   This goes through `pkg/plex/db`, which already reads `media_parts` this
   way. The API has no documented filter by guid.
3. No row means Plex has not matched the item yet. That is not an error: the
   partial scan of its file will match it.
4. Debounce per item for 60 s, then:

   ```
   PUT /library/metadata/{id}/refresh
   ```

   For a Series, refreshing the show refreshes its seasons and episodes.

Only guids under a configured provider identifier are looked up.

### 6.4 RBAC

When `plex.clustarr.enabled` is true, the chart renders a `Role` and
`RoleBinding` in `plex.clustarr.namespace`. They grant the manager's
ServiceAccount `get`, `list` and `watch` on:

- `mediafiles`, `movies`, `series` and `episodes` in `catalog.clustarr.io`;
- nothing else, and no write verbs.

## 7. PMS API client (`pkg/plex/api`)

A small typed client for the calls above, beside the existing
`/identity`, `/library/sections` and maintenance calls:

- it sends `X-Plex-Token` and `Accept: application/json`;
- it caps response bodies (1 MiB), using the repo's existing pattern if one
  exists and adding it here if not;
- it returns a sentinel for 409 and for 404.

The maintenance fan-out keeps its own calls. Moving them onto this client is
not part of this change.

## 8. Testing

- **Provisioner**, against an `httptest` PMS that records calls:
  - an empty server gets exactly one POST per provider, group and library;
  - a second run issues zero writes;
  - a 409 on a provider counts as success;
  - the live shape (providers 9 and 10, groups 7 and 8, `Movies` on group 1)
    with `switchAgent: false` issues no writes and sets the drift gauge;
  - with `switchAgent: true` it issues the PUT with `agent` and then the
    forced refresh;
  - a provider root answering 503 leaves the run `pending`, with no library
    writes for that provider.
- **Watcher**, against a fake dynamic client and a fake PMS:
  - the initial sync issues no scans;
  - a path change scans both folders;
  - 60 changes in one folder make one scan;
  - 51 folders make one section refresh;
  - an unmappable path is counted and dropped;
  - a metadata change with a matched guid refreshes the item;
  - one without a matched guid does nothing.
- **Field paths** are tested against YAML copies of clustarr objects under
  `test/data/clustarr/`, taken from a real cluster, so a clustarr rename
  fails by name.
- **End to end (`make e2e`):**
  - The kind overlay adds a fixture provider serving a `MediaProvider` root
    for one movie, plus the CRDs of `catalog.clustarr.io` (applied from a
    pinned copy).
  - Creating a MediaFile makes Plex scan its folder.
  - The library comes up on the fixture agent without any UI step.

## 9. Rollout on kind-cluster-plex

1. Mount clustarr's library claim into the Plex pods (§3). Downloads on that
   cluster are paused because it is attached to the live library, so this
   step needs the owner's go-ahead.
2. Deploy with `libraries` naming `Movies` (`switchAgent: false`) and `TV`.
   - The provisioner adopts providers 9 and 10 and groups 7 and 8.
   - It creates `TV` on group 8.
   - It reports `Movies` as drifted.
3. When the owner decides, set `switchAgent: true` on `Movies`.

## 10. Open points

- The exact guid PMS stores for a custom-provider match is inferred from
  clustarr's provider, not observed. The first match in step 2 of §9 settles
  it with one read-only query; §6.3 changes only its format string if it
  differs.
- Whether `POST /library/sections` accepts several `location` parameters in
  the old form is not documented. The test against a real PMS in `make e2e`
  settles it, and §5.3 falls back to the `/all` form.
