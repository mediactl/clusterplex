# The guide rig on kind

A throwaway cluster-plex on the owner's workstation, built for the Live TV
guide work of 2026-11-04 to 2026-11-10: the guide loads in PostgreSQL, the
one guide writer, and the managers' own build of a Plex cloud guide
(`docs/superpowers/specs/2026-11-10-guide-writer-and-built-cloud-guide-design.md`,
whose "As built" sections hold what each run found). This page is how to
stand it up again, what its tools under `hack/guiderig` do, and what the rig
taught that the design does not repeat.

## What it is

- **The cluster:** `hack/kind.sh up`, a one-node kind cluster `cluster-plex`
  with its LoadBalancer helper. Everything lives in namespace `media`.
- **Plex:** `k8s/overlays/kind`, with PostgreSQL from `hack/kind-postgres.yaml`
  (pod `plex-postgres`, user `plex`, database `plex`, the shim's schema
  `plex`). The overlay pins `CLUSTERPLEX_PLEX_MACHINE_IDENTIFIER` to a
  value whose processed identifier **is the owner's live server**: the rig
  must run under an identifier of its own
  (`kubectl -n media set env sts/plex CLUSTERPLEX_PLEX_MACHINE_IDENTIFIER=<uuid>`)
  before it is ever claimed. It was `cafe0001-4000-4000-8000-c1a5ca77e001`
  for the guide loads and `c493110e…` once claimed for the cloud guide.
- **The image:** the working tree, `DOCKER_BUILDKIT=1 docker build --target debug
  -t ghcr.io/mediactl/cluster-plex:<tag> .` then
  `kind load docker-image --name cluster-plex ghcr.io/mediactl/cluster-plex:<tag>`
  and `kubectl -n media set image sts/plex plex=…:<tag>`. The shim comes
  from the Dockerfile's `PLEX_PG_REF` (a published plex-postgresql bundle),
  so a shim change is a bundle release first.
- **The manager's config** is the ConfigMap `clusterplex-config` (key
  `config.yaml`), applied with
  `kubectl -n media create cm clusterplex-config --from-file=config.yaml=<file> --dry-run=client -o yaml | kubectl -n media apply -f -`
  and a `rollout restart sts plex`. The guide work used four shapes of one
  file: `plex.liveTV.enabled: true` with `clustarr.namespace: media`;
  plus `cloudGuide: manager` (the built guide, `cloudGuideBuild: job` or
  `inProcess`, `cloudGuideJob` sizing the Job); plus `severalPods: true`
  with the StatefulSet at two replicas; or without `cloudGuide`, so PMS
  loads the guide itself (the load the oracle's reference comes from). A
  `FriendlyName` preference names the rig in plex.tv.
- **The tuner** is `hack/hdhrprobe` with `--plexmap`: an HDHomeRun device
  whose `/plexmap.json` is clustarr's cloud-block map, read from a file the
  ConfigMap `rig-plexmap` mounts (`kubectl -n media create cm rig-plexmap
  --from-file=plexmap.json=<file> --dry-run=client -o yaml | kubectl apply -f -`;
  the probe serves the new file within a minute). `/channels?n=` sets how
  many channels it lists. The map is a JSON list of
  `{channelKey: "<lineup id>-<gridKey>", deviceIdentifier: "<number>",
  lineupIdentifier: "<vcn>"}`, one entry per channel, built from the
  lineup's station list (`lineup-channels.json`, hidden stations skipped,
  one entry per vcn and per gridKey). The rig ran 12 channels and 248.
- **The provider** is an IPTVProvider `rig-cloud` (`spec.type: M3U`,
  `playlistURL` on the probe) **whose status is written by hand**, since no
  clustarr runs on the rig: `status.address`, a Ready condition, and one
  block with `start`, `deviceID` (the probe's), `uri` (the probe's Service
  address), `lineup` (the cloud lineup's bare 24-hex id) and `lineupHash`.
  The converger adopts the block, registers the device, creates the DVR,
  saves the map, and under `cloudGuide: manager` builds the guide; a new
  `lineupHash` makes it save the map again and build again.
- **Claimed or not.** An unclaimed PMS creates DVRs and loads XMLTV guides
  without a token; the cloud guide service needs an account token, which
  PMS gets by being claimed (Secret `plex-claim`, key `claim`, a fresh
  https://plex.tv/claim token, a pod restart within four minutes; delete the
  Secret after). A claimed PMS answers nothing without a token, so every
  probe then sends the server's own (`PlexOnlineToken` in `Preferences.xml`,
  read inside a shell and never printed). Take the rig off the account when
  the run ends (`DELETE /myplex/account` on its API, or plex.tv's Authorized
  Devices).

## Capturing what PMS fetched

The oracle compares a build with PMS's own load from the same answers, so
the answers are captured **while PMS loads**, each grid week the moment PMS
asks for it:

1. An ephemeral container tails PMS's log:
   `kubectl -n media debug plex-0 --image=busybox:1.37-musl --target=plex
   --container=pmstail -- tail -n0 -F "/proc/1/root/var/lib/plexmediaserver/Library/Application Support/Plex Media Server/Logs/Plex Media Server.log"`.
2. `kubectl -n media logs -f plex-0 -c pmstail | python3 -I hack/guiderig/capture.py <fixture> <tokenfile>`
   writes `grid/<gridKey>-<start>.json` and `lineup-channels.json` as PMS
   requests them (with the token swapped in; PMS signs its own requests
   with `X-Plex-Api-Token`, which the service refuses from anyone else),
   records every metadata id in `meta-ids.txt`, and logs each request's PMS
   time in `capture.log`, which `hack/guidebuild` reads as the moment each
   channel was read (`Input.ReadAt`).
3. Then the map save (a new `lineupHash` on the provider's status) starts
   the load; PMS logs `Total time to load EPG` at its end.
4. `python3 -I hack/guiderig/fetchmeta.py <fixture> <tokenfile>` fetches
   the shows and movies afterwards, three a second; `refetch.py` fills a
   missed week or the lineup.

The reference is then the schema PMS loaded (`epg_<dvr uuid>`), copied aside
with `CREATE TABLE epg_ref_x.t AS TABLE epg_<uuid>.t` for the five guide
tables and `library_sections`. The rig keeps `epg_ref_pms2` (12 channels,
the 2026-11-10 capture) and `epg_ref_big` (248 channels, the same day); the
fixtures are in the session scratchpad of that day, not in the repo.

## The oracle

`hack/guidebuild` builds a guide from a fixture into a schema and compares
it with a reference schema in every row, column and id (its header says
what is and is not compared). Two wrappers:

- `hack/guiderig/oracle-rig.sh <tag>` checks the **manager's own build** on
  the rig: it copies the installed guide aside, exports the manager's cache
  (`clusterplex.epg_cache`: the lineup, the grids and the metadata it
  fetched) as a fixture, keeps the plan's two weeks, and builds from that at
  the install's time. `SCHEMA` names the installed guide's schema and
  `GUIDEBUILD_DSN` the port-forwarded database.
- `hack/guiderig/oracle-talos.sh` does the same **read-only on a live
  cluster**: the DDL, the guide's tables and the cache cross as gzipped
  `kubectl exec` dumps (a plain stream dropped bytes once) into a local
  `postgres:15-alpine`, where `pg_trgm` and `unaccent` must exist in schema
  `plex` before the DDL, and the build runs there.

A run is exact when the compare prints no differing table or column. The
guide's "What PMS writes" rules were settled this way, one disagreement at a
time, at 12 channels and then at 248.

## The XMLTV oracle

An XMLTV DVR's built guide (`plex.liveTV.xmltvGuide: manager`) is checked
the same way: `hack/guidebuild -xmltv` builds from the XMLTV file PMS
loaded and compares with PMS's own load of it. No capture is needed, since
the XMLTV is the whole answer. The rig's probe (2026-11-10/11) is DVR 8,
"clustarr x1", device `CAFE0011`, schema `epg_fde0a670482b45e8941c59cf6c7385c3`:

- **The tuner** is `xmltvprobe`, an nginx serving the ConfigMap
  `xmltvprobe-x1` at `/hdhr/x1` (`discover.json`, `device.xml`,
  `lineup.json`, `lineup_status.json`, `xmltv.xml`), and the IPTVProvider
  `rig-x1` (`spec.type: M3U`) carries one hand-written block pointing at it
  (`uri: http://<xmltvprobe ClusterIP>/hdhr/x1`, `guidePath: /xmltv.xml`,
  `guideHash`, `lineupHash`).
- **The guide** is `hack/guiderig/xmltv-gen.py <dir>/site/hdhr/x1 <base URL>`:
  48 probe programmes over four channels, an hour each from the next hour,
  plus the device's JSON; `--profile` appends three listings of clustarr's
  profile (The Drew Barrymore Show's S07E41 on 9001 without plex guids,
  again on 9002 with its `plex`, `plex.show` and `plex.season` guids and
  images, its S07E40 on 9003 without). Serve it with
  `kubectl -n media create cm xmltvprobe-x1 --from-file=<dir>/site/hdhr/x1 --dry-run=client -o yaml | kubectl apply -f -`
  and wait until the pod serves it (`kubectl -n media exec deploy/xmltvprobe -- sha256sum /usr/share/nginx/html/hdhr/x1/xmltv.xml`,
  about a minute).
- **PMS's load.** Under `xmltvGuide: pms` (the default), PMS loads the guide
  itself: `POST /livetv/dvrs/8/reloadGuide` through a forward of
  `plex-0:32400` with the server's `.LocalAdminToken` (read inside the pod by
  a `kubectl debug --target=plex` container whose log is written to a 0600
  file, never printed); the manager copies the load into the schema, and its
  comment (`obj_description`) names the file's new mtime. 48 programmes load
  in seconds. Check the forward first: `GET /identity` must name the rig's
  `c493110e…`, since `127.0.0.1:32401` on the workstation is a Docker PMS.
- **The reference** is that load copied aside, never edited in place:
  `CREATE TABLE epg_ref_x1.<t> AS TABLE epg_fde0….<t>` for the five guide
  tables and `library_sections` (PMS's XMLTV load makes a section only when
  a programme needs it, so the sections come from the reference). The load's
  time is `min(created_at)` of its non-section items.
- **The run:**
  `go run ./hack/guidebuild -dsn "$(cat dsn)" -xmltv <dir>/site/hdhr/x1/xmltv.xml -lineup <dir>/site/hdhr/x1/lineup.json -ref epg_ref_x1 -out epg_ref_x1_built -now <load time> -additions`,
  where `dsn` is a 0600 file holding the DSN of the forwarded
  `plex-postgres` (`hack/kind-postgres.yaml`'s user and database).
  Without `-additions` the run lists every plex guid as a row only one side
  has and every image PMS stores none of as a differing column, and exits 1;
  with it those are listed apart, under "additions", and a run with no other
  difference exits 0. On 2026-11-11 both of PMS's loads were exact so:
  the 48 programmes (110 items, 48 airings, 33 tags, 119 taggings; 1 plex
  guid, 7 arts, 4 thumbs added) and `--profile` (114, 51, 33, 127; 4 plex
  guids, 9 arts, 5 thumbs).
- **What it settled:** PMS sorts a title by a fixed table, not by stripping
  accents (`Ünïcødé` as `Unic0de`), measured with
  `hack/guiderig/xmltv-sortprobe.py`, an XMLTV of 728 one-character titles
  loaded the same way (`pkg/plex/guidebuild/testdata/sortprobe.tsv` keeps
  PMS's answers). The planner takes the one table for cloud guides too, on
  the inference that PMS computes `title_sort` in one place for both
  loaders (the service's answers carry no sort title). The cloud loads on
  the rig (48,548 and 3,942 titles) agree with the table but cannot tell it
  from the old fold, since none carries a character on which the two
  differ; the probe on PMS is the evidence. And the reader keys an item by any plex guid the guide
  lists for it, taking each added image from the first listing that has one.

**The built mode on the rig.** With an image carrying the builder (`kind
load` it as above), set `xmltvGuide: manager` beside `cloudGuide` in
`clusterplex-config` (the base's Role grants no Jobs; the kind overlay
takes `k8s/components/guide-build-jobs`, the chart's `batch` `jobs` get,
list, create and delete for `clusterplex-sa`, without which a build Job
fails "cannot get resource jobs" and waits its 15-minute retry; on the rig
apply only that component's Role and RoleBinding, picked out of
`kubectl kustomize k8s/overlays/kind`, never the whole overlay, whose
StatefulSet pins the production identity), restart the StatefulSet, and
move the block's
`guideHash` in `rig-x1`'s status
(`kubectl -n media patch iptvprovider rig-x1 --subresource=status --type=json -p '[{"op":"replace","path":"/status/blocks/0/guideHash","value":"<new>"}]'`).
The converger asks the builder for DVR 8; a `plex-guide-build-*` Job stages
the build and the holder swaps it in ("Live TV: built an XMLTV guide in a
Job"); the schema's comment then reads `clusterplex: built ...`. Check it
three ways: the oracle against the installed guide at the build's own time
(copy it aside, then `-xmltv <same file> -ref <copy> -out <check> -now
<its min(created_at)>`, exact with no `-additions`, both sides being
builds); PMS's own view, `GET /tv.plex.providers.epg.xmltv:8/grid?type=4`
(the episodes' `thumb`, `art`, `grandparentArt` and `plex://` guids); and a
forced `POST /livetv/dvrs/8/reloadGuide`, which PMS loads and the swap hook
refuses (`GUIDE_SWAP: the manager answered "skip the managers build this
guide"` in the pod's log), the schema's comment and `guidesum.sh` sums
unchanged.

Run on 2026-11-11 (image `68e1ab6`, the `--profile` guide): the converger's
first pass asked for DVR 8 under the moved `guideHash` (the plan at
`plan/<dvr>` names it), the Job `plex-guide-build-fde0a670-546076f66dd89d53`
staged 4 channels, 114 items and 51 airings in 9 s, and the holder swapped
it in 10 s after starting it. The independent build at the install's time
was exact; the grid showed Element Soup's and Image Elements' stills,
backdrops and show backdrops, and The Drew Barrymore Show's episode 41 as
`plex://episode/6aeb5f12ac56869440d7304f` under
`plex://show/5fc69a487c6557002ed60976`, where PMS's own load had shown
neither an image beyond the show's poster nor a plex guid; the forced
reload was refused in 164 µs and the sums stood two minutes later. PMS
still moves the DVR's `refreshedAt` for the load it was refused, so
`refreshedAt` says nothing about whose guide is served. The schema's
comment then read "built from Plex's guide service", the cloud build's
text; an XMLTV build's reads "built from clustarr's XMLTV at …" since,
and either is read as the build's time. Two traps: a
port-forward and the server's `.LocalAdminToken` both die with the pod, so
re-forward and re-read the token after every restart; and the managers'
timers run on the monotonic clock, which stops while the workstation
sleeps, so a 15-minute retry after a suspend comes 15 awake minutes later.

## Checks

- `hack/guiderig/guidesum.sh <schema>`: a count and an md5 of every row of
  each guide table. Taken before and after PMS's boot refresh of a built
  guide, equal sums are the proof that PMS changed nothing.
- `hack/guiderig/mem.sh` (run on the kind node with
  `docker exec -i cluster-plex-control-plane sh -s < hack/guiderig/mem.sh`):
  each Plex pod's manager RSS and its container's cgroup peak. The in-process
  build at 248 channels peaked at 603 MB, which is why the build runs in a
  Job by default.
- PMS's own view: `GET /livetv/epg/grid?…` (the What's On grid) and the
  DVR's `refreshedAt` against the schema's rows, through the pod's port
  with the server's token.

## What the rig taught

- **Hand-written status must carry the real producer's values.** The rig's
  block named the lineup by its bare id; clustarr's tuner names it by PMS's
  URI (`lineup://tv.plex.providers.epg.cloud/<id>#<title>`), and the first
  build on talos failed on it after 52 minutes of fetching (`cloudLineupID`).
  Shape a fixture like the producer, not like the answer.
- **The kind node's etcd shares the host's disk.** A download or swap storm
  on the workstation stalls etcd for minutes, kills every port-forward and
  loses the lease; `crictl exec` into `plex-postgres` on the node
  (`docker exec cluster-plex-control-plane crictl exec -i <container> psql …`)
  still works, and that is how the reference schemas were dumped.
- **`kubectl exec` can drop bytes on a large stream.** Dump through `gzip`
  and check with `gzip -t`.
- **Never run the shim at DEBUG through a load** (3.3 GB of trace, etcd
  timeouts); time a load with PostgreSQL's `log_statement`, not the shim's.
- **Never claim a rig that runs the overlay's pinned identifier**: it would
  re-register the owner's live server. Give the rig its own first.
- **A load's cost is statement volume.** PMS rewrites a show's taggings per
  airing; 12 channels are about 267,000 statements, and a 248-channel load
  took 1,427 s against a 2m47s build.

Tear down with `kubectl delete ns media`, or the whole cluster with
`hack/kind.sh down`.
