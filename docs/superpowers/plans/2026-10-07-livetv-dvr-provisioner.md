# Live TV DVR Provisioner (cluster-plex) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** For each block of each clustarr `IPTVProvider`, cluster-plex's
leader registers an HDHomeRun device in PMS, creates its DVR and saves its
channel map. A block holds up to ~450 channels, so a provider of any size
is several DVRs with contiguous numbers (spec §3.1, amended 2026-10-07).

It also:
- reloads each guide on change;
- removes a block's DVR when the block or provider goes;
- restarts PMS on the other pods after a DVR change;
- records what it did as Kubernetes Events on the provider.

Every Plex pod serves the blocks' ports on a link-local address, through
the supervisor's TCP proxy (spec §5.0).

**Architecture:**
- **`pkg/plex/api`** gains the Live TV calls, pinned by responses
  recorded from the owner's PMS.
- **`pkg/plex/livetv`** gets two parts:
  - a converger: one idempotent pass over the providers, in the style of
    `pkg/plex/provision`;
  - a watcher of its own, a dynamic informer on
    `iptvproviders.clustarr.io` kept separate from `pkg/clustarrwatch` so
    that a missing CRD can never stall the catalog watch.
- **`cmd/manager`** runs it from `startLeaderWork` beside
  `provisionLoop`, and records Events through a `client-go`
  `EventBroadcaster`.

**Tech Stack:** Go, client-go v0.29 (dynamic informer, `tools/record`),
`net/http`, testify, httptest.

**Spec:** `/home/appkins/src/mediactl/clustarr/docs/superpowers/specs/2026-10-07-iptv-live-tv-design.md`.
- §6.1 and §5.0's cluster-plex side are this plan's part. §3.1 says why a
  provider is several DVRs.
- **Amended 2026-10-07** (main `195d1ef7`, `006e8bca`) and approved:
  - blocks and link-local ports;
  - one Lease per block;
  - the guide-load rule;
  - restarting PMS on the other pods.
- **What is built:** Tasks 2-4 were built for one device per provider
  (`b666889` on `livetv`), and Task 1 is recorded (`1cdf7cb`, `1793865`).
  The tasks below amend that code.
- The owner approved it on 2026-10-07, including §10.1: deleting a
  provider removes its DVR and that DVR's recording rules.
- clustarr's half is `/home/appkins/src/mediactl/clustarr/docs/superpowers/plans/2026-10-07-livetv-phase1.md`.
  Its Task 1 defines the fields read here.

## Global Constraints

- **The clustarr fields this plan reads,** as unstructured, importing no
  clustarr code (CLAUDE.md):
  - `spec.enabled`, a bool where absent means true;
  - `spec.epgSource`: `XEPG` (also when absent) or `PMS`;
  - `status.address`, the tuner Service;
  - `status.blocks[]`: `start`, `deviceID`, `port`, `friendlyName`,
    `guidePath`, `lineupHash`, `guideHash`;
  - `status.conditions[type=Ready].status`.
- **Never delete a DVR, or save a map or reload a guide again, while
  `GET /activities` lists a `provider.epg.load`.** One such delete
  deadlocked PMS (recorded).
- **The link-local address** (`plex.liveTV.localAddress`, default
  `169.254.47.1`) is the only address Plex is given for a block.
- **Tests after implementation (the owner, 2026-10-07):** each task is
  implemented first; Task 7 writes and runs every test.
- **Plex is configured through its API only,** never its database.
- **Only the Lease holder writes Plex's config.** The loop runs inside
  `startLeaderWork` alone.
- **Deletion is a recorded exception.** The provisioner otherwise "creates
  and updates; it never deletes" (`docs/configuration.md:409`, CLAUDE.md).
  Live TV deletes, under the owner's 2026-10-07 ruling, and only:
  - the DVRs and devices it manages (rule M below);
  - for a deleted IPTVProvider, or as a duplicate or orphan row on a
    managed device.
  Task 4 amends both documents to say so.
- **Rule M, what is managed (amended):** a PMS device is managed when its
  URI host is the link-local address, or its DeviceID is a live block's.
  A DVR is managed when it is on a managed device, or when its lineup URL's
  host is the link-local address (a ghost). Nothing else is ever touched.
- **Never register a device whose identifier PMS already lists.**
  Registering again is how duplicate and empty DVR rows arise.
- **The channel map is one PUT, never batched.** PMS replaces the whole
  map on each PUT.
- **Tests:** table-driven with testify, named by behaviour.
  `go test ./...` stays hermetic.
- **Logging:** `slog`, one handler per component.
- **Import aliases:** `plexapi`, `plexlivetv`.
- **Commits:** style `feat(scope): …`; pathspec commits after `git add`
  of new files; never `git stash`; never push.
- **Deploying** to kind-cluster-plex is a separate `deploy:` commit, and
  only on the owner's OK.
- **The token:** never print, log or commit the PMS token. Recorded
  fixtures carry `TOKEN` in its place.

## Review Focus

1. **A block splits** (clustarr adds a block in the middle of the
   lineup). One device and one DVR are added, the old block's map is saved
   once, and PMS restarts once on each other pod, and no other DVR is
   touched. Covered by Task 7, `TestASplitAddsOneDVRAndRestartsTheOthersOnce`.
2. **A guide is loading when a delete is due.** Nothing is deleted on
   that pass, `DVRWaitingForGuide` is recorded, and the delete happens on
   a later pass. Covered by Task 7, `TestNothingIsDeletedWhileAGuideLoads`.
3. **Another installation holds a block's Lease.** No device or DVR is
   written for that block, and the others converge. Covered by Task 7,
   `TestABlockAnotherInstallationOwnsIsLeftAlone`.
4. **A provider turns Ready=False with its last good blocks kept**
   (clustarr's overlap rule). No DVR is deleted. Covered by Task 7,
   `TestANotReadyProviderIsLeftAsItIs`.
5. **A Plex pod starts before the IPTVProvider watch syncs.** PMS still
   starts within 30 s, and the proxies follow without restarting it.
   Covered by Task 7, `TestPMSStartsWhenTheFirstSyncIsLate`.

---

### Task 1: Record the Live TV calls against the owner's PMS (spike; needs the owner's OK)

This task creates and deletes a test DVR on the owner's real Plex.
**Ask the owner before Step 3,** and wait for an explicit yes.

**Files:**
- Create:
  - `hack/hdhrprobe/main.go`, a standalone HDHomeRun device with N
    synthetic channels;
  - `hack/hdhrprobe/Dockerfile`;
  - `hack/record-livetv.sh`;
  - `pkg/plex/api/testdata/livetv/*.json`, the recorded responses with the
    token scrubbed;
  - `docs/livetv-pms-calls.md`, the findings.

- [ ] **Step 1: Write the probe.**
  - **Flags:** `--addr :5004`, `--channels 100`, `--base http://<its Service address>`.
  - **What it serves:**
    - `/discover.json`, with DeviceID `CAFE0001`, FriendlyName
      `clustarr probe`, TunerCount 2, ModelNumber `HDTC-2US` and
      FirmwareName `hdhomeruntc_atsc`;
    - `/lineup_status.json`;
    - `/lineup.json`: N entries, `GuideNumber` `1000+i` and `GuideName`
      `Probe i`;
    - `/device.xml`;
    - `/livetv/probe/probe/xmltv.xml`: one channel per number, with one
      2-hour programme each;
    - `/channels?n=` changes N at run time.
  - **The image:** `FROM scratch`, a static binary built with
    `CGO_ENABLED=0`.
  - **Its test:** `go vet ./hack/hdhrprobe`, plus one test that
    `/lineup.json` has N entries.
- [ ] **Step 2: Write `hack/record-livetv.sh`.**
  - **Getting in:** it port-forwards `svc/plex-main` and reads the token
    inside the shell. `TOKEN=$(kubectl exec plex-0 -c plex -- …Preferences.xml…)`
    pulls `PlexOnlineToken`, and is never echoed.
  - **Calling:** it `curl`s each call with `-H 'Accept: application/json'`
    and `-H "X-Plex-Token: $TOKEN"`.
  - **Writing:** it pipes each response through
    `sed "s/$TOKEN/TOKEN/g"` into `pkg/plex/api/testdata/livetv/<step>.json`.
  - **The steps, in order:**
    1. `GET /media/grabbers/devices`
    2. `POST /media/grabbers/devices/discover?uri=…`
    3. `POST /media/grabbers/devices?uri=…`
    4. `GET /media/grabbers/devices`, again
    5. `POST /livetv/dvrs?language=eng&device=<uuid>&lineup=lineup://tv.plex.providers.epg.xmltv/<url-encoded xmltv URL>%23probe`
    6. `GET /livetv/dvrs`
    7. `GET /livetv/epg/channelmap?device=<uuid>&lineup=<lineup id>`
    8. `PUT /media/grabbers/devices/<key>/channelmap?channelsEnabled=…&channelMappingByKey[..]=..&channelMapping[..]=..` for every channel
    9. `GET /livetv/dvrs/<key>`
    10. `POST /livetv/dvrs/<key>/reloadGuide`
  - **The size test:** for N in 100, 480, 481, 1000, 2000 and 5000, it sets
    the probe's N, then repeats steps 7-9. It records the status code, the
    URL length, and the enabled-channel count PMS then reports.
    - It also records whether step 7 shows the new N without a rescan. If
      not, it tries `POST /media/grabbers/devices/<key>/scan` and records
      that.
  - **Cleanup:** `DELETE /livetv/dvrs/<key>`, then
    `DELETE /media/grabbers/devices/<key>`, then a final `GET` of each.
- [ ] **Step 3: Ask the owner, then run it.**
  - Deploy the probe to `media` as a one-replica Deployment and Service:
    `kind load` the image into kind-cluster-plex, then `kubectl apply` a
    manifest kept in the script.
  - Run the script, and delete the probe afterwards.
  - Check `git diff --stat`: only the testdata changed. Grep for the token
    prefix and for `X-Plex-Token=`; neither may appear.
- [ ] **Step 4: Write `docs/livetv-pms-calls.md`.**
  - each call, with its exact method, path, parameters, and the JSON keys
    the code needs;
  - the largest N that one PUT carried, and the PUT's URL length at that
    N;
  - whether PMS needs a rescan before step 7;
  - what PMS mode needs from the wizard.
  - **If a size above 480 failed,** stop and report to the owner: spec
    §3.1's fallback (several devices) then becomes the plan's next work,
    in place of Tasks 2-4 as written.
- [ ] **Step 5: Commit.** `git add hack/hdhrprobe hack/record-livetv.sh pkg/plex/api/testdata/livetv docs/livetv-pms-calls.md && git commit -m "docs(livetv): PMS's Live TV calls, recorded against PMS 1.43.4 -- the probe, the script, the fixtures and the channel-map size limit" -- hack/hdhrprobe hack/record-livetv.sh pkg/plex/api/testdata/livetv docs/livetv-pms-calls.md`

### Task 1b: Prove link-local devices and a proxied stream (spike; needs the owner's OK)

Task 1's recording settled the calls (`docs/livetv-pms-calls.md`). The
spec as amended (§5.0) adds four claims to prove on the owner's PMS before
Tasks 5-6 are built:
- **A link-local address:** PMS registers a device at
  `http://169.254.47.1:<port>`.
- **One address, two ports:** two devices on that address, at once, with
  different DeviceIDs.
- **A proxied stream:** a stream plays through a proxy port.
- **The other pods:** whether a channel-map change on the lease holder
  shows on another pod that already knows the DVR, without a restart.

**Files:**
- Modify:
  - `hack/hdhrprobe/main.go`: `--device-id`, plus `--listen` that may
    repeat, so one probe serves several devices;
  - `hack/record-livetv.sh`: a `link-local` mode.

**How it runs:**
- On the lease holder and on one other pod, an ephemeral debug container
  (`--profile=netadmin`, an image already on the node) shares the pod's
  network namespace. There it adds `169.254.47.1/32` to `lo`, and runs the
  probe on `169.254.47.1:47000` and `:47001`.
- Then the script registers both devices and creates both DVRs, and plays
  one stream through `curl` from the debug container.
- It restarts PMS on the other pod (a restart is the only refresh), then
  changes the map on the lease holder and reads the other pod's channels.
- It removes everything under the guide-load rule.

**Before it runs:** ask the owner, and wait for an explicit yes. It
creates and deletes two DVRs and restarts one pod's PMS.

- [ ] **Step 1:** the probe and script changes.
- [ ] **Step 2:** ask, then run.
- [ ] **Step 3:** add the findings to `docs/livetv-pms-calls.md`. If PMS
  refuses a link-local or same-address device, stop and report: §5.0 then
  needs another address scheme. Commit with
  `git commit -m "docs(livetv): link-local devices, two on one address, a proxied stream" -- hack docs`.

### Task 2: `pkg/plex/api` as recorded

The calls exist (`b666889`). Make them match the recording.

**Files:** `pkg/plex/api/livetv.go`

**The changes:**
- **`Device`:** `Key Str`, `UUID`, `URI`, `DeviceID` (`deviceId`),
  `Model` (`model`, the FriendlyName), `State`, and `ParentID int`
  (`parentID`, a number). `Name` goes.
- **`DVR`:** `Key Str`, `UUID`, `Language`, `Lineup`, `LineupTitle`
  (`lineupTitle`), `EPGIdentifier` (`epgIdentifier`) and `Devices []Device`
  (`Device`). `Title` goes.
- **`ChannelMapping`:** adds `Favorite` and `Enabled` (`favorite` and
  `enabled`, strings).
- **`DiscoverDevice` goes.** The converger never calls it (the recording:
  `size: 0`).
- **New: `Activities(ctx) ([]Activity, error)`,** with
  `Activity{UUID, Type, Title, Subtitle string; Progress int}` from
  `GET /activities`.
- **New: `MapBytes(m []ChannelMapping) int`.** It is the length of
  `SaveChannelMap`'s encoded query, computed from the same `url.Values`.
  `SaveChannelMap` refuses a query over `MaxChannelMapBytes = 30000` with
  `ErrChannelMapTooLarge`, before any request.
- **Never an empty map:** `SaveChannelMap` refuses an empty `m` with
  `ErrEmptyChannelMap`. A save without the map cleared it in the
  recording.

- [ ] **Step 1:** implement. `go build ./... && go vet ./pkg/plex/api/`.
- [ ] **Step 2:** commit. `git commit -m "feat(plexapi): the Live TV types as recorded, Activities, and a channel map refused when empty or over 30,000 bytes" -- pkg/plex/api`.

### Task 3: The converger, per block

**Files:** `pkg/plex/livetv/converge.go`

**Interfaces:**
- Produces:
  - **`Provider`:** `{Namespace, Name, UID string; Ready, Enabled bool; EPGSource, Address string; Blocks []Block}`.
  - **`Block`:** `{Start, DeviceID, FriendlyName, GuidePath, LineupHash, GuideHash string; Port int32}`.
  - **`Converger`** gains:
    - `LocalAddress string`, default `169.254.47.1`;
    - `Identity string`: this installation's PMS machine identifier;
    - `Leases LeaseClient`:
      `interface{ Acquire(ctx, namespace, name, identity string, labels map[string]string) (held bool, holder string, err error) }`,
      implemented in Task 4 over `coordination.k8s.io` Leases.
  - **`Result`** gains `DVRChanged bool`: a DVR was created or deleted on
    this pass.
  - **New reasons:**
    - `DVRBlockOwned`;
    - `DVRDeviceIDTaken`;
    - `DVRWaitingForGuide`: a delete, save or reload postponed by a guide
      load;
    - `DVRChannelMapTooLarge`.

**`Run`, per pass:**
1. **Read PMS once:** the devices, the DVRs, and the activities.
   `guideBusy` is any activity of type `provider.epg.load`.
2. **For each Ready, enabled provider, each block in `start` order:**
   1. **The Lease:** `livetv-<lower-case DeviceID>` in the provider's
      namespace, labelled `clustarr.io/iptv-provider=<name>` and
      `clustarr.io/iptv-block=<start>`, acquired as `Identity`. When
      another holder has it, record `DVRBlockOwned`, and skip the block.
   2. **The device:** identifier `device://tv.plex.grabbers.hdhomerun/<DeviceID>`.
      - **Absent:** `AddDevice("http://<LocalAddress>:<Port>")`, then list
        again.
      - **Present at another URI:** record `DVRDeviceIDTaken`, and skip.
   3. **The DVR:**
      - **Under XEPG with none:** `CreateDVR(Language, uuid, XMLTVLineup("http://<LocalAddress>:<Port><GuidePath>", FriendlyName))`,
        and set `DVRChanged`.
      - **Under PMS:** `DVRNeedsLineup` as before.
      - **More than one DVR on the device:** keep the lowest key and delete
        the rest, but only when not `guideBusy`.
   4. **The map,** when the saved hash differs: `EPGChannelMap`, then
      `SaveChannelMap`, but only when not `guideBusy`, else
      `DVRWaitingForGuide`. Over budget: record `DVRChannelMapTooLarge`,
      which clustarr's blocks prevent.
   5. **The guide,** when its hash differs: `ReloadGuide`, under the same
      rule.
3. **Managed rows that no block names:** devices whose URI host is
   `LocalAddress` but whose DeviceID is in no live block, and DVRs on them.
   That covers a removed block and a gone provider. They are deleted, DVR
   first, only when not `guideBusy`, and set `DVRChanged`.
4. **Ghosts, every pass:** a DVR whose lineup URL host is `LocalAddress`,
   on no managed device, is deleted under the same rule.
5. **Rule M, amended:** a device is managed when its URI host is
   `LocalAddress` or its DeviceID is a live block's. Nothing else is
   touched.

- [ ] **Step 1:** implement over the built converger. `go build ./... && go vet ./pkg/plex/livetv/`.
- [ ] **Step 2:** commit. `git commit -m "feat(livetv): converge per block -- one Lease per block, a device on the link-local proxy port, nothing deleted or saved while a guide loads, ghosts swept every pass" -- pkg/plex/livetv`.

### Task 4: Watcher, Leases, wiring, RBAC, config

**Files:**
- Modify:
  - `pkg/plex/livetv/watch.go`: `ProviderOf` reads `status.address` and
    `status.blocks[]`;
  - `cmd/manager/livetv.go`;
  - `cmd/manager/config_provision.go`;
  - the chart, `k8s/components/clustarr/rbac.yaml`, and
    `docs/configuration.md`.
- Create: `pkg/plex/livetv/leases.go`

**The changes:**
- **`leases.go`:** `LeaseClient` over `coordination/v1`:
  - get the Lease, or create it;
  - take it when its holder is empty, is us, or has expired
    (`renewTime + leaseDurationSeconds`);
  - renew it on every pass. The duration is 10 minutes.
- **Config:**
  - `plex.liveTV.localAddress`, default `169.254.47.1`. It is validated as
    link-local `169.254.0.0/16` other than `169.254.169.254`.
  - `plex.liveTV.restartEvery`, default `10m`.
- **`Identity`:** the server's `machineIdentifier`, from `GET /identity`.
- **RBAC** in clustarr's namespace: `coordination.k8s.io` `leases`
  `get;create;update`, beside `iptvproviders` `get;list;watch`.

- [ ] **Step 1:** implement. `go build ./... && make lint`.
- [ ] **Step 2:** commit. `git commit -m "feat(livetv): block Leases held as the server's identity, the blocks read from clustarr's status, and plex.liveTV.localAddress" -- pkg cmd charts k8s docs`.

### Task 5: The proxies in every Plex pod

**Files:**
- Create:
  - `pkg/plex/livetv/proxies.go`;
  - `pkg/plex/livetv/linklocal_linux.go`, adding the address to `lo`
    through `github.com/vishvananda/netlink`, already in `go.mod`.
- Modify:
  - `cmd/manager/supervisor.go`: the proxies start before PMS;
  - `cmd/manager/main.go`: every pod, not only the lease holder, runs a
    read-only `Watcher` when `plex.liveTV.enabled`.

**Interfaces:**
- `ProxySet{LocalAddress string; Logger *slog.Logger}`
- `(*ProxySet).Apply(providers []Provider)`: one `proxy.TCP` per block,
  listening on `<LocalAddress>:<Port>` and targeting `Provider.Address`.
  Listeners are added and removed as blocks come and go; an existing one
  is never restarted.
- `(*ProxySet).Close()`
- `EnsureLinkLocal(addr string) error`: `netlink.AddrAdd(lo, addr/32)`.
  `EEXIST` is success.

**The order at start:**
1. `EnsureLinkLocal`.
2. The watcher's first sync, waited for up to 30 s. Past that, PMS starts
   anyway, and the proxies follow.
3. `ProxySet.Apply`.
4. PMS.

- [ ] **Step 1:** implement. `go build ./... && make lint`.
- [ ] **Step 2:** commit. `git commit -m "feat(livetv): every Plex pod serves each block's port on the link-local address, through the supervisor's TCP proxy, before PMS starts" -- pkg cmd`.

### Task 6: Restarting PMS on the other pods after a DVR change

Each PMS caches its DVRs from its start (recorded). After a pass with
`DVRChanged`, the lease holder restarts PMS on each other pod, one at a
time, at most once per `restartEvery`.

**Files:**
- Create: `cmd/manager/livetvrestart.go`
- Modify:
  - `cmd/manager/livetv.go`;
  - `cmd/manager/supervisor.go`, so a restart can be requested;
  - the RBAC: core `pods` `get;list;watch;patch` in Plex's namespace.

**How:**
- **The lease holder** writes annotation
  `livetv.clusterplex.io/restart: <generation>` on one other pod. It waits,
  up to 5 minutes, for that pod to write
  `livetv.clusterplex.io/restarted: <generation>`, then moves to the next
  pod.
- **Each pod's manager** watches its own Pod. On a new generation it
  restarts PMS in place (the supervisor's restart, not the pod). Once Plex
  answers for its library again, it writes the `restarted` annotation.
- **Changes during the wait** set a pending flag, which the next round
  takes.
- **The Event:** `DVRPodsRestarted`, on each provider whose DVRs changed.

- [ ] **Step 1:** implement. `go build ./... && make lint`.
- [ ] **Step 2:** commit. `git commit -m "feat(livetv): after a DVR change the lease holder restarts PMS on each other pod in turn, at most once per 10 minutes" -- cmd charts k8s`.

### Task 7: The test pass (the owner's rule: tests once the feature is complete)

Every test the earlier tasks deferred is now written and run against the
recorded fixtures (`pkg/plex/api/testdata/livetv`). A fake PMS shaped like
them backs the converger tests.

- **`pkg/plex/api`:**
  - each call's method, path, query and decoding against its fixture;
  - `MapBytes` of a 454-mapping map is 32,701;
  - over 30,000 bytes is `ErrChannelMapTooLarge`, with no request;
  - an empty map is `ErrEmptyChannelMap`;
  - `Activities` decodes `provider.epg.load`.
- **The converger:**
  - a provider of three blocks gets three Leases, devices on
    `169.254.47.1:<port>`, three DVRs, and one PUT per block;
  - a second pass writes nothing;
  - a block's new hash saves only that block's map;
  - a new block adds one device and one DVR, and sets `DVRChanged`;
  - a removed block's DVR and device are deleted;
  - with `provider.epg.load` listed, no delete, save or reload happens, and
    `DVRWaitingForGuide` is recorded; the next pass completes;
  - another installation's live Lease gives `DVRBlockOwned` and no write;
    an expired one is taken;
  - a DeviceID at another URI gives `DVRDeviceIDTaken` and no write;
  - a ghost DVR whose lineup host is the link-local address is deleted;
    a real HDHomeRun and its DVR are never touched;
  - a not-Ready provider is left as it is;
  - a failed pass retries without duplicates.
- **The watcher:**
  - `ProviderOf` reads `status.blocks[]`;
  - live and gone sets are reported;
  - a missing CRD never stalls the catalog watch.
- **`ProxySet`:**
  - blocks added and removed open and close listeners on the given
    address. Test it on `127.0.0.1` with distinct ports, since
    `EnsureLinkLocal` needs `CAP_NET_ADMIN`; that one is covered by
    `make test-netns`, which exists for network namespaces;
  - a connection is piped to the target, and an existing listener
    survives an `Apply` that keeps its block.
- **The restarts:**
  - one round restarts each other pod in turn, the next only after the
    previous answers `restarted`;
  - a second change within `restartEvery` waits;
  - the lease holder never restarts itself.
- **Wiring:** the lease holder runs Live TV and stops when it lets go;
  Events land on the provider.

- [ ] **Step 1:** write them. Falsify each once, by reverting the line it
  guards and watching it fail by name.
- [ ] **Step 2:** `go test -race ./...` and `make lint`. Expected: PASS.
- [ ] **Step 3:** commit. `git add` every new test, then
  `git commit -m "test(livetv): the deferred tests -- recorded calls, the per-block converger, Leases, the guide-load rule, ghosts, proxies and restarts" -- pkg cmd`.

### Task 8: Gate and hand-off

- [ ] **Step 1: Gate.** In a clean worktree, run `go test ./...` and
  `make lint`. Expected: exit 0.
- [ ] **Step 2: The end-to-end check.** It needs clustarr's Part A2
  deployed on kind-cluster-plex, so it needs the owner's OK. Do not
  deploy. Report as ready, and name the overlay changes it will need:
  - `plex.liveTV.enabled: true`;
  - the `newTag` bump, as its own `deploy:` commit.
