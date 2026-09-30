# Cluster Plex

Plex Media Server on Kubernetes, scaled horizontally. The library lives in a
shared PostgreSQL database that every pod reads and writes, a proxy tier serves
the media bytes so aggregate bandwidth is not capped by one node's interface,
and a shim replaces Plex's helper binaries so transcodes run on other pods.

Go module `github.com/mediactl/clusterplex`. One image, four binaries.

## Read before designing anything

- `docs/media-proxy-pattern.md` — the proxy tier: why it serves bytes itself,
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
| `cmd/proxy/` | The media proxy clients connect to |
| `cmd/shim/` | Stands in for Plex's helper binaries and forwards each invocation to the manager |
| `cmd/maintenance/` | What a CronJob runs to ask the manager to distribute one task |
| `pkg/plex/db/` | The shared PostgreSQL library database |
| `pkg/plex/bootstrap/` | Preparing the PostgreSQL schema, the SQLite shadows and Plex's state directory before Plex starts (upstream's init script, in Go) |
| `pkg/plex/net/` | Plex's network namespace and the plex.tv egress filter |
| `pkg/plex/prefs/` | Merging settings into Plex's `Preferences.xml` |
| `pkg/plex/route/` | Finding the pods to send traffic to |
| `pkg/lease/` | Leader election on a Kubernetes Lease |
| `pkg/maintenance/` | The task catalogue and the fan-out across pods |
| `pkg/hashring/` | Consistent hashing, for both the fan-out and session pinning |
| `pkg/mediaproxy/`, `pkg/proxy/` | Serving media, and the L4 proxy |
| `pkg/remoteexec/` | Running helper binaries locally or on a worker pod |
| `pkg/plex/api/` | A client for Plex's admin API: providers, agents, libraries, refreshes |
| `pkg/plex/provision/` | Reconciling configured providers, agents and libraries into Plex |
| `pkg/clustarrwatch/` | Following clustarr's catalog: folder rescans and item refreshes |

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
  Per-pod on an `emptyDir`: `Plug-in Support/Databases/` (the shim's shadow,
  rebuilt every start), `Logs/`, and the transcode directory. Sharing one of
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
- **Media analysis and skip markers are seeded into Plex's database**
  (ADR 0006), the one exception to API-only: Plex's API can write neither.
  `pkg/plexseed` writes clustarr's probe into an unanalysed media item and
  reconciles TheIntroDB's markers, only in the provisioned libraries, never
  over Plex's own analysis, with every row marked (`cp:source`,
  `pv:source=theintrodb`). Configuration stays API-only.
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
  it can see. Set `plex-external-url` (chart: `proxy.externalURL`) for an
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
  2026-09-30, where `k8s/overlays/kind-cluster-plex` runs the integration).
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
