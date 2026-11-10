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
