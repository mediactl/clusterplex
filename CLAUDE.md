# Cluster Plex

Plex Media Server on Kubernetes, scaled horizontally. The library lives in a
shared PostgreSQL database that every pod reads and writes, every pod runs
Plex and serves the clients one LoadBalancer Service pins to it, and a shim
replaces Plex's helper binaries so transcodes run on other pods. An opt-in
media proxy tier can serve the media bytes itself.

Go module `github.com/mediactl/clusterplex`. One image, four binaries.

## Read before designing anything

- `docs/media-proxy-pattern.md` — the opt-in media proxy tier (not in
  `k8s/base`; chart `proxy.enabled`, off): why it serves bytes itself,
  and how sessions are pinned to pods.
- `docs/configuration.md` — the config system, the Plex preferences an operator
  may set, and the ones this architecture fixes.
- `docs/adr/0003-isolate-plex-in-a-network-namespace.md` — why Plex runs in its
  own network namespace, and why the proxy can therefore hold 32400 itself.
  Supersedes `0002`, which is kept for why the proxy is L4 and for the 32401
  discovery.
- `docs/adr/0005-retire-the-media-proxy-for-gateway-api.md` — **rejected**.
  Sessions are pinned by the Service, not by a Gateway `HTTPRoute`. Read it for
  why `sessionPersistence` is the wrong mechanism, why the Gateway cannot hold
  the address Plex advertises, and why retiring the byte offload is still an
  open question that now needs an ADR of its own.
- `docs/adr/0004-run-plex-on-every-pod.md` — why the lease elects the plex.tv
  owner rather than the pod that runs Plex, and which parts of Plex's state
  stop being shared so every pod can run one. Read it before touching the
  volumes or the election.
- `docs/adr/0001-litefs-over-mvsqlite.md` — **historical**. It is why LiteFS was
  chosen over mvsqlite, and why Plex cannot run as several coordinated instances
  on replicated SQLite. The library has since moved to PostgreSQL, which is what
  made active mode possible; read it for the reasoning, not the current design.

## Layout

| Path | Owns |
| --- | --- |
| `cmd/manager/` | Runs in every pod: leader election, the Plex supervisor, the maintenance fan-out, the job listeners |
| `cmd/proxy/` | The opt-in media proxy tier (chart `proxy.enabled`, off by default; `k8s/base` does not run it) |
| `cmd/shim/` | Stands in for Plex's helper binaries and forwards each invocation to the manager |
| `cmd/maintenance/` | What a CronJob runs to ask the manager to distribute one task |
| `pkg/plex/db/` | The shared PostgreSQL library database |
| `pkg/plex/bootstrap/` | Preparing the PostgreSQL schema, the SQLite shadows and Plex's state directory before Plex starts (upstream's init script, in Go) |
| `pkg/plex/net/` | Plex's network namespace and the plex.tv egress filter |
| `pkg/plex/prefs/` | Merging settings into Plex's `Preferences.xml` |
| `pkg/plex/claim/` | Exchanging a plex.tv claim code for the server's token |
| `pkg/plex/servertoken/` | Claiming before Plex starts, and holding every replica to the plex.tv account in clustarr's Secret (the master copy once it holds a token) |
| `pkg/plex/route/` | Finding the pods to send traffic to |
| `pkg/lease/` | Leader election on a Kubernetes Lease |
| `pkg/maintenance/` | The task catalogue and the fan-out across pods |
| `pkg/hashring/` | Consistent hashing, for both the fan-out and session pinning |
| `pkg/mediaproxy/`, `pkg/proxy/` | Serving media, and the L4 proxy |
| `pkg/remoteexec/` | Running helper binaries locally or on a worker pod |
| `pkg/plex/api/` | A client for Plex's admin API: providers, agents, libraries, refreshes |
| `pkg/plex/codecs/` | Having Plex download the audio decoders a Live TV tune needs, through a short audio-only transcode of a library item, on the lease holder |
| `pkg/plex/activity/` | What Tautulli reads from Plex, read by the manager: streams, plays, watch time, library counts, as Prometheus series and a span per finished play |
| `pkg/plex/provision/` | Reconciling configured providers, agents and libraries into Plex |
| `pkg/clustarrwatch/` | Following clustarr's catalog: folder rescans and item refreshes |
| `pkg/remux/` | Plex Web's DASH streams on the remux pool: classify and parse a shimmed job, the manifest, the fMP4 splitter (`remux`), the cache (`cache`), the ffgo pipeline (`pipeline`), the worker's gRPC server (`worker`), the manager's relay (`relay`) |
| `cmd/remux-worker/` | The remux pool's binary; links ffgo |
| `pkg/video/` | Plex's video conversions on the GPU pool (ADR-0010): parse and classify a shimmed job, Plex's segment cuts and lists (`video`), the Lease that counts them (`lease`), the device measure (`gpu`), the ffgo pipeline muxing in memory (`pipeline`), the worker's gRPC server (`worker`), the manager's relay (`relay`) |
| `cmd/video-worker/` | The GPU pool's binary; links ffgo |

## Invariants

- **The library is PostgreSQL and every pod shares it.** There is no database
  primary to elect and nothing to replicate.
- **One pod talks to plex.tv.** The Lease decides which. Every pod runs under
  one server identity, so several pods publishing at once makes that identity
  appear to move between addresses. Pods that do not hold the Lease have their
  route to plex.tv dropped in nftables.
- **The Lease is the only source of truth for leadership.** Do not reintroduce a
  derived copy such as a pod label; a cache can disagree, and one did.
- **Every pod runs Plex, and the Lease changes nothing about that.** Winning it
  opens a route to plex.tv; losing it closes one. Neither starts or stops Plex.
  There is no `plex-mode`: a pod is serving or it is broken, never up and
  deliberately idle. See ADR-0004.
- **Plex's state is split, and the split is load-bearing.** Shared on the
  ReadWriteMany claim: `Preferences.xml`, `Metadata/`, `Media/`, `Cache/`.
  Per-pod on an `emptyDir`: `Plug-in Support/Databases/` (the shim's shadows,
  rebuilt every start; the Live TV guides' too, each served from a schema
  `epg_<DVR uuid>` of its own), `Logs/`, and the transcode directory. Sharing one of
  the per-pod ones corrupts the library rather than failing loudly.
- **Plex's `Preferences.xml` is merged, never regenerated, and under a lock.**
  It holds the server identity and the plex.tv token, which cannot be
  reconstructed, and every pod merges into the one copy as it starts.
- **Settings the architecture depends on are forced, not defaulted.** The Butler
  schedulers, `PublishServerOnPlexOnlineKey`, `ManualPortMappingMode` and
  `customConnections` are written on every start and *refused* as configuration.
  See `pkg/plex/prefs/required.go` and the table in `docs/configuration.md`.
- **Plex's configuration is provisioned through its API, never its
  database.** Providers, agents and libraries are created (never deleted)
  by the Lease holder from `plex.metadataProviders`/`plex.libraries`; a
  library on another agent is only reported unless `switchAgent` is set.
  The clustarr watch reads clustarr as unstructured objects and imports
  none of its code.
- **Live TV is the provisioner's one deletion** (owner, 2026-10-07).
  `pkg/plex/livetv`, behind `plex.liveTV`, adds each of clustarr's
  IPTVProviders to Plex as a DVR: one device registration, the DVR, and the
  full channel map in one PUT (more than the wizard's 480 channels).
  - It deletes only the DVRs and devices of managed HDHomeRun devices: a
    deleted provider's, duplicates, and orphans named by their lineup URL.
  - It never registers a listed device again.
  - Its own informer keeps a missing CRD from stalling the clustarr watch.
  - The PMS calls are in `docs/livetv-pms-calls.md`, recorded against the
    owner's PMS.
- **Media analysis and skip markers are seeded into Plex's database**
  (ADR 0006), the one exception to API-only: Plex's API can write neither.
  `pkg/plexseed` writes clustarr's probe into an unanalysed media item and
  reconciles TheIntroDB's markers, only in the provisioned libraries, never
  over Plex's own analysis, with every row marked (`cp:source`,
  `pv:source=theintrodb`). Since 2026-10-27 it also writes a movie's and a
  show's **Labels** from clustarr's `status.metadata.keywords` (TMDB's)
  through `plexseed.ReconcileLabelsTx` on the chain's first row at every
  apply: PMS keeps no Label from a metadata provider (clustarr's spike,
  2026-10-08), and a Plex refresh rewrites only what the provider answers,
  so labels written here stay. A Label is tag_type 11
  (`plexseed.LabelTagType`, verified on the owner's server with one probe
  label through the API), one `tags` row per name shared by every item,
  each of our taggings marked `cp:source`; a label the owner set in Plex is
  never touched, the same name included. The keywords are part of the
  item's chain key (`Node.Labels`, `clustarrwatch.chainKey`), so a change
  re-applies the item's files. Configuration stays API-only.
- **Only `cmd/remux-worker` and `cmd/video-worker` link ffgo.** The
  manager, shim, proxy and maintenance binaries are static and run from a
  scratch image; ffgo (purego) would make them dynamic.
  `TestNoStaticBinaryLinksFFgo` holds it. Browser streams reach the remux
  pool through `pkg/remux/relay` (ADR-0007).
- **A video conversion never runs on Plex's node** (owner, 2026-10-26,
  ADR-0010). Every job that converts video goes to the GPU pool through
  `pkg/video/relay`. When the pool cannot take it, it is refused: there is
  no fallback to Plex's transcoder. The pool writes nothing to disk; it
  muxes in memory and streams back to the relay.
- **Scheduling lives in Kubernetes, not in the manager.** Maintenance is
  CronJobs calling `/api/v1/maintenance/{task}`, so a call that fails during a
  failover is a failed Job that retries and shows up in `kubectl`.

## Commands

```bash
make build         # manager, shim, proxy and maintenance into bin/
make test          # unit tests
make test-netns    # pkg/plex/net against real network namespaces (needs unshare)
make lint          # golangci-lint v2
make docker-build  # the image (FROM scratch); DOCKER_TARGET=debug adds busybox
make docker-buildx IMG=ghcr.io/mediactl/cluster-plex:<tag>  # publish for linux/amd64 and linux/arm64 (PLATFORMS); needs a builder that runs arm64
# CI publishes both images on every push to main (tags: the 7-char sha, `main`, `latest`; `v*` tags their semver):
# .github/workflows/images.yml builds each platform natively on its own runner and merges the digests, so
# `make docker-buildx` is for an image CI has not built (a branch, a shim ref: `gh workflow run images.yml -f plex_pg_ref=...`).
make helm-lint     # lint and render the chart
make docker-build DOCKER_TARGET=debug  # the e2e suite execs sh, cat and find in the pod
make kind-up kind-load deploy-kind
make e2e           # deploys the kind overlay and runs the end-to-end test
make deploy-kind-clustarr  # the kind overlay plus clustarr stand-ins; refuses a cluster running a real clustarr
```

The end-to-end test is behind the `e2e` build tag so `go test ./...` stays
hermetic. The nftables blocklist is only covered by `make test-netns`.

## Gotchas found the hard way

- **Plex binds 32400 *and* 32401.** It exits with "Error binding acceptor:
  Address in use" if either is taken, within a tenth of a second and with the
  reason only in its own log. This no longer collides with the proxy, because
  Plex binds them inside its own namespace — but it is exactly why provisioning
  that namespace is fatal on failure rather than best effort. Plex started in
  the pod namespace lands on the proxy's port and dies this way.
- **Plex has no setting for its listen address or port.** It is confined with a
  network namespace rather than steered with a rule; see `pkg/plex/net` and ADR
  0003. Nothing shells out for it and the image carries no `iptables`.
- **Never set `Pdeathsig` on a process started in the namespace.** It fires when
  the *forking thread* exits, and that thread is a temporary one the Go runtime
  retires whenever it likes — so Plex would be killed at random. `pkg/plex/net`
  rejects it rather than letting it be set.
- **Plex advertises `169.254.1.2` to plex.tv,** because that is the only address
  it can see. Set `plex-external-url` (chart: `plex.externalURL`) for an
  address clients can reach; see `docs/configuration.md`.
- **`ProcessedMachineIdentifier` is what clients see as the server ID.** Plex
  derives it from `MachineIdentifier` with a salt you cannot reproduce, and
  **never recomputes it**. Change the UUID without deleting the derived value
  and clients keep seeing the old server forever.
- **A helper binary only reaches the library if the manager gives it
  `LD_PRELOAD`.** The shim removes it from Plex's environment once it has
  loaded, so that Plex's ordinary children do not inherit a musl-linked
  library, and re-injects it only when it sees Plex exec a scanner itself. It
  never sees ours: Plex execs `cmd/shim`, which forwards the call to the
  manager, and the manager starts the real binary. Without the preload the
  helper opens the per-pod SQLite shadow instead of PostgreSQL, finds nothing
  in it, and **exits 0 having done nothing** — so `Plex Media Scanner
  --analyze` leaves `media_streams` empty and playback fails with
  `s1001 (Network)`, the server having logged "video has neither a video
  stream nor an audio stream". `execServices` sets `Executor.Env` for this
  reason. The scanner's own log is the tell: with the preload it reaches
  "Analyzing media parts", without it stops at "Opening 20 database sessions".
- **A provisioned section's rows are a projection of clustarr's MediaFiles,
  and Plex's scanner never runs for it (ADR-0008, 2026-10-15).**
  `pkg/plexproject` writes `metadata_items` (keyed by guid under one rule,
  `plexproject.GUID`, with no `hash` so Plex fills the rest on refresh),
  `directories`, `media_parts`, `media_items` (stamped `cp:mediaFile=<uid>`,
  `media_analysis_version` 8) and, through `pkg/plexseed.SeedTx`, the
  streams and markers, from the MediaFile the watcher hands it; a path or
  probe change updates the same row; a deleted file trashes its rows and
  the items left empty, and an item keeping another version keeps no
  deleted one beside it (`plexproject.clearSuperseded`, after every delete,
  move and apply; a `cp:missing` row stays for the sweep to put back), since
  Plex showed such a row as the item's missing file until the empty-trash
  job ran (every upgraded episode, talos 2026-10-27). A node's row is found by every key it may carry
  (`plexproject.locate`): its guid or prior custom form, a movie's with any
  `/edition/<title>`, a season's or an episode's place under its parent,
  and the row the file's media hangs off while stamped for the same item
  (`cp:itemUID`). Never look one up by a single exact guid: Plex's refresh
  rewrites a row's guid to whatever the provider answers, and the exact
  lookup left 73 bogus movie editions and 454 episode twins in Plex's trash
  on talos (2026-10-16 to 27). A file that moves to another item
  soft-deletes its row there instead of carrying it along, since PMS's Empty
  Trash removes a trashed item only through its deleted media rows
  (captured on stock PMS 1.43.5). An episode is keyed by its Plex id only
  inside a season Plex knows (`plexproject.EpisodePlexID`, the provider's
  own rule). A movie file's `spec.edition` is Plex's edition: `edition_title`
  and the guid `<guid>/edition/<path-escaped title>`, under which Plex keys
  the edition's watched state, so a re-key moves it.
  Plex 1.43 scans a library inside its own process,
  so the shim never sees a scan: the proxy catches the plain-HTTP
  `POST /library/sections/{id}/refresh` and sends it to `--scan-route`
  (the manager's `/api/v1/scan`, 404 for an unprovisioned section so the
  request goes on to Plex, a reconcile on the lease holder, forwarded
  there from any other pod), the 6-hour resync reconciles every section,
  and `Plex Media Scanner --analyze --item N` -- the only thing the binary
  still does -- is answered exit 0 for a projected item. A TLS client's
  scan still reaches Plex until the proxy terminates with Plex's
  `Cache/cert-v2.p12`; that walk changes nothing once the projection is
  current. Debug with `SELECT extra_data FROM plex.media_items WHERE
  extra_data LIKE '%cp:mediaFile%' LIMIT 3` and
  `curl -X POST http://<pod>:8080/api/v1/scan?section=1`. The field paths
  the watcher reads are declared once per kind (`clustarrwatch.FieldPaths`)
  and checked against clustarr's CRDs (`TestEveryFieldPathExistsInClustarrsCRDs`),
  which found the watcher reading a `seriesRef` no MediaFile has.
- **The scanner authenticates with `.LocalAdminToken`, not `X_PLEX_TOKEN`,
  so analysis works only on the pod whose Plex started last.** Plex writes a
  fresh local token to that file on every start (`SafelyWriteFile`: a temp
  file renamed over it, so neither a symlink nor a file mount survives), the
  file is on the shared claim, and `Plex Media Scanner` reads it for the
  `X-Plex-Token` it sends to `127.0.0.1:32400` -- the path is built in the
  binary, and `X_PLEX_TOKEN` in its environment does not change it. The
  other pods' Plex have never seen that token: Plex logs `Refreshing tokens
  inside the token-based authentication filter`, a plex.tv lookup with
  `unknownToken=`, `authenticating user as guest`, `Signed-in Token ()` and
  a 401; the scanner logs `Unable to allocate a changestamp from the
  server` and **exits 0**, so `media_streams` stays empty and playback fails
  with `s1001` exactly as a missing `LD_PRELOAD` does. Proven on
  kind-cluster-plex, 2026-09-30: one `PUT /library/metadata/N/analyze` with
  the owner's token, sent to each pod, analysed on the last-started pod and
  401'd on another, both jobs inside Plex's namespace. Helpers first ran in
  the pod namespace, where Plex logged the call-back as `169.254.1.1
  (Subnet)`, and that was taken for the cause; moving them into Plex's
  namespace (`remoteexec.Executor.Start` is `plexnet.Network.StartProcess`
  for every job) made the log say `Loopback` and changed nothing else. The
  file's token is accepted from the subnet too. Not fixed yet.
- **"The recording conflicts with other recordings" can be a grab nobody
  watches, not a schedule clash.** PMS ends a Live TV grab only when its
  client reports that playback stopped. A client that is killed or goes
  offline mid-play reports nothing, so its grab keeps a tuner until PMS
  restarts. With the account's one connection, that blocked every tune on
  talos for six hours (2026-10-20). The tell is PMS's `Timed out waiting
  for available tuner (waited 300 seconds)` while clustarr's relay reports
  `clustarr_livetv_tuners_in_use 0`.
  - `pkg/plex/livetv.StaleGrabs` cancels a Live TV grab's activity after
    `plex.liveTV.staleGrabAfter` unwatched: `DELETE /activities/<uuid>`,
    the only call that ends a grab.
  - Two calls that don't: `DELETE /livetv/sessions/<id>` is 404, and a
    stopped timeline only counts from the grab's own client.
- **PMS loads a Live TV guide into a file of its own and renames it into
  place.** A guide reload copies Plex's bundled database to
  `<guide>-loading.db`, fills it, and renames it onto the guide's file
  (talos, 2026-10-21: the guide file was born when the load began). The shim
  goes by file name, so a load never reaches the guide's schema on its own.
  The manager copies each load in (`plexbootstrap.ImportGuide`,
  `cmd/manager/guides.go`): a staging schema, swapped in by renaming, so PMS
  never waits on the swap. See "The guide, in PostgreSQL" in
  `docs/configuration.md`.
- **Live TV's "Could not tune channel" can be a missing audio decoder,
  not the tuner.** Plex's transcoder ships without AAC, MP2 and MP3
  decoders, among others. PMS downloads each into `Codecs/<transcoder
  build>-<platform>/` on the config volume, but only when it transcodes a
  file it has analysed. A Live TV stream is never analysed, so a tune never
  downloads one.
  - **The tell:** the transcoder logs `[stream_segment,ssegment] sample
    rate not set` and exits 234, then PMS logs `Recorder: Error 16`.
  - **When it happens:** a new config volume, a PMS upgrade into a new
    folder, or a new architecture. talos's arm64 Plex had no AAC decoder
    until 2026-10-20.
  - **The fix:** `pkg/plex/codecs` has the lease holder ask for an HLS
    transcode of a library item that copies the video and converts the
    audio, and fetch its first segment. Fetching is the only proof: the
    variant playlist lists every segment of the file from the start. It asks as "Plex for Android (TV)"
    with HEVC appended, because Plex Web's profile copies only H.264, and a
    decision that converts the video (allowed since 2026-10-26,
    `TranscoderCanOnlyRemuxVideo=0`; until then refused with decision 4010,
    "maximum simultaneous video transcodes") is refused. It records what
    it fetched in `Codecs/.clusterplex-prefetch.json` per platform and PMS
    version.
  - **Pods of another architecture** share the volume but not the
    decoders. They get theirs only while one of them holds the Lease.
- **The PostgreSQL shim is lossy in two known ways.** Library search returns
  nothing, because it translates Plex's full-text `MATCH` into a constant false
  predicate; and title ordering follows PostgreSQL's default collation, because
  Plex's `naturalsort` collation has no equivalent. Both are upstream, in
  `cgnl/plex-postgresql`.
- **The shim must be built against the musl Plex bundles,** which is why its
  Dockerfile stage is pinned to Alpine 3.15 rather than something current.
  Upstream publishes no Linux binaries.
- **Viper lowercases nested map keys.** `FriendlyName` silently becomes
  `friendlyname`, and Plex preference names are case sensitive. Preferences are
  a list of name/value pairs for that reason.
- **Plex can die without the supervisor noticing.** It runs under a subreaper
  that stays alive while any descendant does — that is what stops Plex's
  vfork re-exec looking like an exit — so `cmd.Wait` keeps blocking and
  `OnUnexpectedExit` never fires. The health watch is the only thing that
  finds out, and what it does about it is `Supervisor.Restart`, not exiting.
  The pod is still replaced once Plex has been restarted `plexRestartLimit`
  times inside `plexRestartWindow`. A corollary: `plexHealth.record` fires
  once per *run* of failures, not once ever, or a Plex that never comes back
  would be left alone at 0/1.
- **Plex's crash dumps are deleted on every start,** by the manager's
  bootstrap (`pkg/plex/bootstrap`), to keep CrashUploader from running. A crash
  therefore leaves no dump behind by the time anyone looks at the pod, and
  "no crash report" says nothing about whether Plex crashed. The host's
  kernel log is the record that survives: `dmesg -T | grep segfault` names
  the faulting thread and library.
- **A stale `plexmediaserver.pid` stops Plex starting.** It outlives the
  container on a persistent volume and container PIDs repeat, so the supervisor
  removes it before every start.
- **PMS 1.43.4 answers 404 to `POST /library/sections/all`,** the form its
  own API docs give for creating a library. `plexapi.CreateSection` falls
  back to `POST /library/sections?type=movie&location=…`, which works and
  honours `metadataAgentProviderGroupId`. A clustarr match is stored in
  `metadata_items.guid` as `<provider identifier>://<movie|show|…>/<uid>`,
  exactly `clustarrwatch.Guid`'s form (both seen on kind-cluster-plex,
  2026-09-30).
- **kind-cluster-plex runs Plex from clustarr's Helm chart, not from this
  repo's manifests** (2026-10-08). Its `clustarr` release installs this chart
  as the `cluster-plex` subchart in `clustarr-system`, pinned by version from
  `oci://ghcr.io/mediactl/charts` (`make chart-push`; bump
  `charts/cluster-plex/Chart.yaml` for every chart change). The
  `k8s/overlays/kind-cluster-plex` overlay that ran it in `media` until then
  was deleted: applying it now would start a second Plex with the same
  machine identifier and plex.tv token. Roll a new image there with
  `helm upgrade clustarr` and `cluster-plex.image.tag`.
- **The chart and `k8s/base` are two installs of one design, and they
  drifted.** By chart 0.2.0 the chart's client Service still selected the
  media proxy Deployment, its Plex port was `pms` where the base targets
  `proxy`, and its Role could not delete a Lease. `test/manifests`
  (`parity_test.go`) now holds the chart's Plex pods, client and headless
  Services, PDB and Role to the base: change both, or the test names the
  difference. Its render helpers open every file under `k8s/` and the chart
  (`trackInputs`), because kubectl and helm read them in a subprocess the
  `go test` cache cannot see, and an edited template once passed on a cached
  result.
- **`go test ./...` passing does not mean the cluster works.** Nearly every bug
  in this repo so far was only visible in a real cluster.
- **The image is `FROM scratch`: no shell, no `cat`, no `curl`.** Plex's
  binaries carry their own musl loader and libraries, ours are static, and
  what used to need bash, psql, sqlite3 and python3 -- upstream's init script
  -- is `pkg/plex/bootstrap`. `kubectl exec` therefore finds nothing to run;
  build with `DOCKER_TARGET=debug` for busybox in `/bin`, or attach one with
  `kubectl debug --target=plex` and read files through `/proc/1/root`. Plex
  does call `/bin/sh` -- its plug-in host's Python, looking for a C compiler
  when `uuid` is imported -- and copes without one: the same plug-ins start,
  with the same warnings (checked 2026-09-30 by booting Plex in both images
  with a logging stub as `/bin/sh`).
- **Plex's bundled ffmpeg is stripped of `lavfi`,**
  so generate test media on the host and copy it in. Plex writes a local admin
  token to `.LocalAdminToken` in its state directory, which is the way to call
  the API on a claimed server.

## Conventions

- Logging is `slog` with a handler per component; no package-level logger.
- Packages under `pkg/plex/` are named for their directory (`db`, `net`,
  `prefs`, `route`, `bootstrap`) and imported as `plexdb`, `plexnet`,
  `plexprefs`, `plexroute` and `plexbootstrap`: `net` would shadow the
  standard library and `db` the usual variable name. Alias every import the same way, so call sites read alike.
- Formatting rules for every file type are in `.editorconfig`; line endings
  and generated or vendored paths are in `.gitattributes`.
- Tests are table-driven with testify, and name the behaviour rather than the
  function. Anything worth a comment about *why* goes in the test name or a
  comment above it.
- Commit messages are plain imperative sentences, matching this repo's history.
