# Live TV DVR Provisioner (cluster-plex) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** For each clustarr `IPTVProvider`, cluster-plex's leader registers
the HDHomeRun device in PMS, creates its DVR and saves the full channel
map, which carries more than 480 channels. It also reloads the guide on
change, removes the DVR when the provider is deleted, and records what it
did as Kubernetes Events on the provider.

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
- §6.1 is this plan's part, and §3.1 says why there is no channel cap.
- The owner approved it on 2026-10-07, including §10.1: deleting a
  provider removes its DVR and that DVR's recording rules.
- clustarr's half is `/home/appkins/src/mediactl/clustarr/docs/superpowers/plans/2026-10-07-livetv-phase1.md`.
  Its Task 1 defines the fields read here.

## Global Constraints

- **The clustarr fields this plan reads,** as unstructured, importing no
  clustarr code (CLAUDE.md):
  - `spec.enabled`, a bool where absent means true;
  - `spec.epgSource`: `XEPG` (also when absent) or `PMS`;
  - `spec.device.friendlyName`;
  - `status.deviceID`, `status.address`, `status.guideURL`,
    `status.lineup.hash`, `status.guideHash`;
  - `status.conditions[type=Ready].status`.
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
- **Rule M, what is managed:** a PMS device is managed when its
  `deviceIdentifier` is `device://tv.plex.grabbers.hdhomerun/<id>`, and
  either:
  - `<id>` is a live provider's `status.deviceID`; or
  - a DVR on it has an XMLTV lineup whose URL path matches
    `^/livetv/<clustarr namespace>/[^/]+/xmltv\.xml$` (the orphan case).
  Nothing else is ever touched.
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

1. **A provider that turns Ready, then not Ready (its playlist fails):**
   nothing is deleted or re-registered, and the DVR stays.
   Covered by Task 3, `TestANotReadyProviderIsLeftAsItIs`.
2. **cluster-plex restarts, and loses the hashes it last saved:** the
   first pass saves the channel map and reloads the guide once each, then
   is idle. Covered by Task 3, `TestAFreshConvergerSavesOnceThenIdles`.
3. **A provider deleted while cluster-plex was down:** its XEPG DVR is
   found as an orphan by its lineup URL and removed. A PMS-mode one cannot
   be found, and is left; Task 4's docs say so.
   Covered by Task 3, `TestAnOrphanXEPGDVRIsRemoved`.
4. **The IPTVProvider CRD is not installed:** the catalog watch still syncs
   and works, and the Live TV watcher logs once and retries.
   Covered by Task 4, `TestAMissingCRDNeverStallsTheCatalogWatch`.
5. **PMS answers 5xx in the middle of a pass:** the pass returns `Pending`,
   records `DVRProvisionFailed` with PMS's snippet, and a retry later
   completes without duplicating anything.
   Covered by Task 3, `TestAFailedPassRetriesWithoutDuplicates`.

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

### Task 2: `pkg/plex/api`: the Live TV calls

**Files:**
- Create: `pkg/plex/api/livetv.go`, `pkg/plex/api/livetv_test.go`

**Interfaces:**
- Consumes: Task 1's fixtures and findings. The JSON keys below are the
  expected ones from iptvtunerr's code. Where a fixture differs, the
  fixture wins: change the struct tags and ledger a ruling.
- Produces:
  - **Types:**
    - `plexapi.Device{Key, UUID, URI, DeviceIdentifier string}`
    - `plexapi.DVR{Key, UUID, Lineup string; Devices []Device}`
    - `plexapi.ChannelMapping{DeviceIdentifier, ChannelKey, LineupIdentifier string}`
  - **Methods:**
    - `(*Client) Devices(ctx) ([]Device, error)`
    - `DiscoverDevice(ctx, uri string) ([]Device, error)`
    - `AddDevice(ctx, uri string) error`
    - `DeleteDevice(ctx, key string) error`
    - `DVRs(ctx) ([]DVR, error)`
    - `CreateDVR(ctx, language, deviceUUID, lineup string) (DVR, error)`
    - `DeleteDVR(ctx, key string) error`
    - `EPGChannelMap(ctx, deviceUUID, lineup string) ([]ChannelMapping, error)`
    - `SaveChannelMap(ctx, deviceKey string, m []ChannelMapping) error`
    - `ReloadGuide(ctx, dvrKey string) error`
  - **Lineups:** `plexapi.XMLTVLineup(guideURL, title string) string`
    returns `"lineup://tv.plex.providers.epg.xmltv/" + guideURL + "#" + title`.
    `url.Values` encodes it; never pre-encode it.

- [ ] **Step 1: Write the failing tests** with the existing `recorder(t, bodies, status)` helper (`pkg/plex/api/client_test.go:16`), feeding it Task 1's recorded bodies, `os.ReadFile("testdata/livetv/<step>.json")`. One test per method asserts:
  - the method, the path, and the exact query keys, values and order;
  - the decoded result against the recorded body;
  - that the token travels in the header.

  Plus two tests:
  - **`TestSaveChannelMapSendsEveryPairInOneRequest`:** 2,000 mappings
    produce one call, whose query holds 2,000 `channelMappingByKey[...]`
    and 2,000 `channelMapping[...]` keys, and a `channelsEnabled` value
    listing all 2,000 identifiers comma-separated.
  - **`TestXMLTVLineupIsEncodedOnce`:** the recorded query of `CreateDVR`
    holds `lineup=lineup%3A%2F%2Ftv.plex.providers.epg.xmltv%2Fhttp%3A%2F%2F10.96.0.5%2Flivetv%2Fmedia%2Fnews%2Fxmltv.xml%23Clustarr+news`.
- [ ] **Step 2: Run.** `go test ./pkg/plex/api/`. Expected: FAIL to compile.
- [ ] **Step 3: Implement.**
  - **The calls:** each method follows `Providers`/`AddProvider` (`client.go:89-103`): an anonymous `MediaContainer` wrapper, `c.do(...)`.
  - **`SaveChannelMap`:** builds `url.Values` with `channelsEnabled` as the
    comma-joined device identifiers, and both indexed keys per mapping.
    - **If Task 1 found** that PMS takes the map only as a form body past
      some size, add an unexported `doForm` beside `do` that sends
      `application/x-www-form-urlencoded`, and use it here. Ledger that.
  - **A slower client:** a Live TV client gets
    `HTTP: &http.Client{Timeout: 2 * time.Minute}` from its caller
    (Task 4), since the 30 s default is short for a large map.
    `pkg/plex/api` keeps its default.
- [ ] **Step 4: Run.** Expected: PASS.
- [ ] **Step 5: Commit.** `git add pkg/plex/api/livetv.go pkg/plex/api/livetv_test.go && git commit -m "feat(plexapi): Live TV device, DVR, channel-map and guide calls, pinned by recorded PMS responses" -- pkg/plex/api`

### Task 3: `pkg/plex/livetv`: the converger

**Files:**
- Create:
  - `pkg/plex/livetv/converge.go`
  - `pkg/plex/livetv/converge_test.go`
  - `pkg/plex/livetv/fakepms_test.go`

**Interfaces:**
- Consumes: Task 2's client.
- Produces:
  - `plexlivetv.Provider{Name, Namespace, DeviceID, Address, GuideURL, FriendlyName, EPGSource, LineupHash, GuideHash string; Ready, Enabled bool}`
  - `plexlivetv.Event{Namespace, Name, Reason, Message string; Warning bool}`
  - The reasons:
    - `ReasonProvisioned = "DVRProvisioned"`
    - `ReasonChannelMapSaved = "DVRChannelMapSaved"`
    - `ReasonNeedsLineup = "DVRNeedsLineup"`
    - `ReasonAddressChanged = "DVRDeviceAddressChanged"`
    - `ReasonFailed = "DVRProvisionFailed"`
    - `ReasonRemoved = "DVRRemoved"`
  - **The converger:**
    - `plexlivetv.Converger{PMS *plexapi.Client; Namespace, Language string; Logger *slog.Logger; Record func(Event)}`
    - `(*Converger).Run(ctx, live []Provider, gone []Provider) (Result, error)`,
      where `Result` is `"converged"`, `"pending"` or `"error"`, as
      `provision.Result`.
  - **Its memory:** the converger keeps
    `saved map[string]struct{ lineup, guide string }`, keyed by device ID,
    in memory only. A restart therefore saves once (Review Focus 2).

- [ ] **Step 1: Write the fake PMS.** `fakepms_test.go`, in the shape of
  `pkg/plex/provision/fakepms_test.go`:
  - **State:** `devices`, `dvrs` and `maps` in memory.
  - **Responses:** answers shaped like Task 1's fixtures. Load the recorded
    bodies as templates where practical.
  - **Writes:** each one appended to `writes []string` as
    `"METHOD /path"`; for the channel-map PUT, as
    `"PUT /media/grabbers/devices/<key>/channelmap n=<pairs>"`.
  - **`down bool`:** gives 502.
  - **Anything unmatched:** 418.
- [ ] **Step 2: Write the failing tests,** each with a `Provider` built in the test:
  - **`TestAReadyXEPGProviderGetsADeviceADVRAndItsChannelMap`:**
    - the writes, in order: discover, `POST /media/grabbers/devices`,
      `POST /livetv/dvrs`, the channelmap PUT with `n` equal to the fake
      EPG map's size, then `POST …/reloadGuide`;
    - the Events: `DVRProvisioned`, then `DVRChannelMapSaved`.
  - **`TestASecondPassWritesNothing`.**
  - **`TestANewLineupHashSavesTheMapOnce`:** a single PUT.
  - **`TestANewGuideHashReloadsTheGuideOnce`.**
  - **`TestAFreshConvergerSavesOnceThenIdles`:** a new converger meets an
    existing device and DVR. It makes one PUT and one reload, then nothing
    on the next pass.
  - **`TestAPMSProviderGetsADeviceOnlyThenIsAdopted`:** first a device
    write and `DVRNeedsLineup`, with no DVR created. After the test adds a
    DVR on that device to the fake (the wizard), the next pass makes one
    PUT.
  - **`TestAnExistingDeviceIsNeverRegisteredAgain`:** the device listed
    under another URI gives `DVRDeviceAddressChanged`, with zero writes.
  - **`TestADuplicateDVROnAManagedDeviceIsRemoved`:** two DVRs on the
    device; the one with the higher key is `DELETE`d, the other kept.
  - **`TestADeletedProviderLosesItsDVRAndDevice`:** with the provider in
    `gone`, the writes are `DELETE /livetv/dvrs/<k>`, then
    `DELETE /media/grabbers/devices/<k>`, and `DVRRemoved` is recorded.
  - **`TestAnOrphanXEPGDVRIsRemoved`:** a DVR whose lineup is
    `…/livetv/<Namespace>/old/xmltv.xml`, with no live or gone provider
    `old`, is deleted with its device.
  - **`TestAnUnmanagedDVRIsNeverTouched`:** a real HDHomeRun device and
    its DVR are never touched; nor is a DVR whose lineup names another
    namespace.
  - **`TestANotReadyProviderIsLeftAsItIs`:** with the provider not Ready,
    or disabled, an existing device and DVR give zero writes.
  - **`TestAFailedPassRetriesWithoutDuplicates`:** the fake goes down
    after the device write, so the pass is `pending` and records
    `DVRProvisionFailed`. Once it is up, the next pass creates the DVR
    once: one device and one DVR in the fake.
  - **`TestTwoThousandChannelsGoInOnePUT`.**
- [ ] **Step 3: Run.** Expected: FAIL to compile.
- [ ] **Step 4: Implement `Run`.**
  1. **Read PMS:** list the devices and DVRs once per pass.
  2. **Index by device ID:** take the ID from `DeviceIdentifier`, after its
     last `/`.
  3. **For each live provider that is Ready and Enabled,** in name order:
     1. **The device.** When it is absent: discover, then add, then list
        again to get its key and UUID. When present with a URI host other
        than `Address`, record `ReasonAddressChanged` once per pass and go
        on to the next provider.
     2. **The DVR.**
        - Under XEPG with no DVR on the device: `CreateDVR(Language, uuid, XMLTVLineup(GuideURL, FriendlyName))`
          and record `ReasonProvisioned`.
        - Under PMS with no DVR: record `ReasonNeedsLineup` and go on to
          the next provider.
        - With more than one DVR on the device: keep the lowest key and
          delete the rest.
     3. **The channel map,** when `saved[id].lineup != LineupHash`:
        `EPGChannelMap(uuid, dvr.Lineup)`, then `SaveChannelMap(device.Key, all)`,
        then record `ReasonChannelMapSaved` with the count. Do any rescan
        Task 1 found necessary before the GET.
     4. **The guide,** when `saved[id].guide != GuideHash` and the hash is
        non-empty: `ReloadGuide`.
  4. **For each gone provider,** and each orphan by rule M: delete its DVRs,
     then its device, and record `ReasonRemoved`.
  5. **Errors:** each is recorded as `ReasonFailed` with the error's text,
     which already holds PMS's `snippet`, and makes the result `pending`.
     The pass goes on to the next provider.
- [ ] **Step 5: Run.** `go test -race ./pkg/plex/livetv/`. Expected: PASS.
- [ ] **Step 6: Commit.** `git add pkg/plex/livetv && git commit -m "feat(livetv): converge each IPTVProvider's Plex device, DVR, channel map and guide -- one PUT per map, never a second registration, deletion only of managed rows" -- pkg/plex/livetv`

### Task 4: The watcher, Events, wiring, RBAC, config and docs

**Files:**
- Create:
  - `pkg/plex/livetv/watch.go`
  - `pkg/plex/livetv/watch_test.go`
  - `cmd/manager/livetv.go`
  - `cmd/manager/livetv_test.go`
- Modify:
  - `cmd/manager/leaderwork.go` (`startLeaderWork`'s early return, and
    `go m.liveTVLoop(ctx)`)
  - `cmd/manager/config_provision.go` (`plex.liveTV.enabled`, default
    false, and `plex.liveTV.language`, default `eng`)
  - `cmd/manager/main.go` (build `Dynamic` when either clustarr or Live TV
    is enabled)
  - `charts/cluster-plex/templates/rbac.yaml` and
    `k8s/components/clustarr/rbac.yaml` (in the clustarr namespace:
    `clustarr.io` `iptvproviders` get;list;watch, and core `events`
    create;patch)
  - `charts/cluster-plex/values.yaml` (`plex.liveTV`)
  - `docs/configuration.md` (a Live TV section, and the deletion exception)
  - `CLAUDE.md` (the same exception, in one paragraph)

**Interfaces:**
- Produces:
  - `plexlivetv.Watcher{Dynamic dynamic.Interface; Discovery discovery.DiscoveryInterface; Namespace string; Logger *slog.Logger; OnChange func(live []Provider, gone []Provider)}`
  - `(*Watcher).Run(ctx) error`
  - `plexlivetv.ProviderOf(u *unstructured.Unstructured) Provider`

- [ ] **Step 1: Write the failing tests.**
  - **`TestProviderOfReadsEveryField`:** an unstructured object with every
    field set, and one with `spec.enabled` and `spec.epgSource` absent,
    which read `true` and `XEPG`.
  - **`TestTheWatcherReportsLiveAndGone`:** with
    `dynamicfake.NewSimpleDynamicClientWithCustomListKinds`, a create, an
    update and a delete call `OnChange` with the right sets. A deletion
    passes its last-known `Provider` in `gone`, read through
    `cache.DeletedFinalStateUnknown` too.
  - **`TestAMissingCRDNeverStallsTheCatalogWatch`:**
    - a fake discovery without `clustarr.io/v1alpha1`, so `Run` logs one
      `iptvproviders.clustarr.io is not installed` line and retries every
      5 min (an injected interval);
    - it never calls `OnChange`, and returns on context cancel;
    - and a `clustarrwatch.Watcher` started beside it still reaches its
      synced state (`Counters.Synced(true)`).
  - **`cmd/manager/livetv_test.go`, `TestTheLeaseHolderRunsLiveTVAndStopsWhenItLetsGo`:**
    modelled on `TestTheLeaseHolderProvisionsAndStopsWhenItLetsGo`
    (`leaderwork_test.go:18`). After `takePlexTV`, a provider in the
    dynamic fake reaches the httptest PMS as a discover call; after
    `releasePlexTV`, no further calls arrive.
  - **`TestEventsAreRecordedOnTheProvider`:** a fake clientset's actions
    include a `create events` in the clustarr namespace, whose
    `involvedObject` is the IPTVProvider (apiVersion `clustarr.io/v1alpha1`,
    kind, name, UID) and whose reason is `DVRProvisioned`.
- [ ] **Step 2: Run.** Expected: FAIL.
- [ ] **Step 3: Implement.**
  - **`watch.go`:**
    - check `Discovery.ServerResourcesForGroupVersion("clustarr.io/v1alpha1")`
      for `iptvproviders`;
    - its own `dynamicinformer.NewFilteredDynamicSharedInformerFactory(…, 0, Namespace, nil)`,
      with no `Trim` transform;
    - every handler calls a debounced (2 s) `OnChange` with the current
      store's providers, plus the gone set since the last call.
  - **`cmd/manager/livetv.go`, `liveTVLoop(ctx)`:**
    - builds a `Converger` with `PMS: &plexapi.Client{BaseURL: "http://" + m.plexAddr, Token: m.plexToken, HTTP: &http.Client{Timeout: 2 * time.Minute}}`;
    - **records Events** through `record.NewBroadcaster()` with
      `StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: m.K8sClient.CoreV1().Events(ns)})`,
      as source component `cluster-plex`. `Record` maps an `Event` to
      `recorder.Event(objRef, type, reason, msg)`, with an
      `ObjectReference` built from the provider's identity, which the
      watcher keeps by name;
    - **runs a pass** on every `OnChange`, and also every
      `m.provisionEvery`, with the same backoff as `provisionLoop`;
    - counts passes in
      `m.Metrics.ProvisionRuns.WithLabelValues("livetv-" + string(res))`.
  - **`startLeaderWork`:** its early return at `leaderwork.go:32` also
    checks `!m.Config.LiveTV.Enabled`, and it adds
    `if m.Config.LiveTV.Enabled && m.Dynamic != nil { go m.liveTVLoop(ctx) }`.
  - **The RBAC:** add the rules to the chart's clustarr-reader Role and to
    the kustomize component's Role.
  - **The docs:**
    - **Live TV in `docs/configuration.md`:** what is managed (rule M);
      the deletion exception and why; that a PMS-mode provider deleted
      while cluster-plex was down is not found; the Events.
    - **One paragraph in CLAUDE.md,** naming
      `docs/livetv-pms-calls.md`.
- [ ] **Step 4: Run.** `go test -race ./...` and `make lint`. Expected: PASS.
- [ ] **Step 5: Commit.** `git add pkg/plex/livetv/watch.go pkg/plex/livetv/watch_test.go cmd/manager/livetv.go cmd/manager/livetv_test.go && git commit -m "feat(livetv): the lease holder converges Plex's DVRs from IPTVProviders and records Events on them -- plex.liveTV config, RBAC, docs (the provisioner's one deletion exception)" -- pkg/plex/livetv cmd/manager charts k8s docs CLAUDE.md`

### Task 5: Gate and hand-off

- [ ] **Step 1: Gate.** In a clean worktree, run `go test ./...` and `make lint`. Expected: exit 0.
- [ ] **Step 2: End-to-end check.** It needs clustarr's phase 1 deployed on
  kind-cluster-plex, and so the owner's OK. Do not deploy. Report as ready,
  and name the overlay change it will need:
  - `plex.liveTV.enabled: true` in
    `k8s/overlays/kind-cluster-plex/kustomization.yaml`'s config;
  - the `newTag` bump as its own `deploy:` commit.
