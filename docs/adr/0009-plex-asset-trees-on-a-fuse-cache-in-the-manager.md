# ADR-0009: Plex's asset trees are served by a FUSE cache inside the manager, backed by an object store

**Status:** Deferred (2026-10-16, the owner): the shared claim moves to the NAS over NFS, which needs no code, and clustarr's object stores stay on NATS, clustered to three servers. Reopen if a measured refresh or poster-wall latency on NFS says otherwise.
**Date:** 2026-10-16
**Deciders:** cluster-plex maintainers, with the owner
**Relates to:** ADR-0001 (the manager has mounted FUSE over Plex's state before), ADR-0004 (every pod runs Plex; the volume split), ADR-0008 (the library is a projection, so bundle contents are reproducible)

## Context

ADR-0004 split Plex's state into what every pod must see one copy of and
what belongs to one Plex process. The shared half rides a ReadWriteMany
claim. On talos (2026-10-16) that claim is a ReadWriteOnce local-path
volume on one node, which is why the release runs one replica: three pods
cannot mount it. The cluster offers no ReadWriteMany class; the Synology
NAS over NFS is the only one available, and the question was whether the
shared half could be served without it.

What the shared half holds was measured on the live pod (PMS 1.43.4,
cluster-plex 23aa7e0):

| Tree | Size | Objects | Written by | Read by |
| --- | --- | --- | --- | --- |
| `Metadata/` | 1.5 GB | 15k files | a refresh on one pod: posters, art and XML fetched from clustarr's provider | every pod that serves the item |
| `Media/` | 167 MB | 12.5k bundles | the same refresh: per-media-item thumbnails | every pod |
| `Cache/PhotoTranscoder/` | 377 MB | 6k files | the first `/photo/:/transcode` for a (source, size) key, in-process through FreeImage | every later request for that key, served as a plain file read (0 ms) |
| everything else | under 10 MB | | `Preferences.xml` by Plex and the manager, both by temp-file-plus-rename; `Cache/*.dat`, `cert-v2.p12` and `Codecs/` by the lease holder, the only pod with plex.tv egress | every pod, at start and on each plex.tv check |

The three trees are the size and the file count. They share three
properties that make them unlike the rest: a file is written whole and
then only read; the same bytes serve every pod; and every one of them is
reproducible, from clustarr's artwork store for the bundles (ADR-0008:
the row is a projection and Plex refills it from the provider) and from
the source image for a photo transcode. Plex reads them as a stat followed
by a read, with far more stats than reads: a poster wall is one stat per
item and a read only for the sizes the client has not seen.

The remainder is not like that. `Preferences.xml` is the identity and the
token, rewritten in place with a rename and guarded by the manager's
`flock`. The plex.tv caches are what let a pod without egress authenticate
users. They stay where ADR-0004 put them, on a shared claim that is now
small enough to be anything: the NAS, or a per-pod copy seeded by the
lease holder, which is a later ADR.

Two precedents bound the design. The manager ran LiteFS's FUSE filesystem
in-process over `Plug-in Support/Databases` from ADR-0001 until the move to
PostgreSQL (`cmd/manager/litefs.go` at `7645732^`): mounted before Plex
started, in the same container and mount namespace, with `allow_other`,
so none of this is new to the pod. And `pkg/proxy`, `pkg/hashring` and
the `plex-workers` headless Service already give the pods each other's
addresses.

## Decision

The manager serves `Metadata/`, `Media/` and `Cache/PhotoTranscoder/` from
a FUSE filesystem of its own, mounted inside the Plex container before
Plex starts, and the three trees leave the shared claim.

1. **One daemon, inside the manager.** No sidecar: the manager already
   owns Plex's start order (`Supervisor.Preferences` runs before Plex,
   the proxy starts before and stops after), its probes, its drain and
   its peer addresses. The filesystem is a package, `pkg/assetfs`, started
   from `run()` after the network namespace and before `runPlex`, and
   unmounted after Plex has exited. go-fuse v2's `fs` API, which LiteFS
   used underneath; not bazil, whose maintenance has lapsed.

2. **Three mounts, one index, one cache.** Each tree is mounted at its own
   path under `Plex Media Server/` so the rest of that directory stays an
   ordinary volume. The three servers share one in-memory index of the
   whole key space (`<tree>/<path>` → size, ETag, mtime, about 40k
   entries) and one cache. `Getattr`, `Lookup` and `Readdir` answer from
   the index and never touch the network, because the stat is the hot
   operation. Directories are implied by their keys and materialised in
   the index, so `Mkdir` then `Create` works with no placeholder object.
   Attributes are fixed: `0644` and `0755`, Plex's uid and gid, size and
   mtime from the object, inode from a hash of the path.

3. **Bytes come from a versioned, peer-to-peer cache; the object store is
   the copy of record.** The cache key is `<path>@<etag>`. A rewrite gives
   a new ETag, so a new key, so nothing is ever invalidated across pods:
   old versions age out under the byte budget. The cache is
   `mailgun/groupcache/v2` (the original has no removal or TTL, and
   `golang/groupcache` is already an indirect dependency): keys are owned
   by consistent hash over the peers, a miss is loaded once per cluster
   with singleflight, and a copy lands in the hot cache of whichever pod
   keeps asking. The getter fetches from an S3-compatible bucket and
   refuses bytes whose ETag does not match the key, correcting the index
   instead, so a stale index can never poison a key. Objects over an
   inline cap (8 MiB; nothing in these trees is near it, BIF generation
   is off) stream from the bucket with range reads and are not cached.

4. **Writes spool locally and upload once, under the final name.** A
   handle opened for writing is a temp file on the pod's `emptyDir`;
   `Flush` uploads it and `Release` is idempotent. `Rename` of a still-open
   writer retargets its destination, so Plex's temp-plus-rename costs one
   `PutObject`, not a put, a server-side copy and a delete. An upload that
   fails returns `EIO`: Plex logs it and refreshes the item later. Nothing
   is spooled for retry; a poster that silently appears an hour late is
   worse than a logged miss.

5. **Index coherence rides the peers, not a bus.** After an upload or an
   unlink the writer posts `{path, size, etag, mtime, deleted}` to every
   peer's `/index` endpoint on the same HTTP server that serves
   groupcache. A lost post is bounded by the one-second attribute and
   entry timeouts the kernel is given and by the getter's ETag check. No
   NATS dependency enters cluster-plex for this.

6. **Kubernetes is the environment, not an afterthought.**
   - **Privilege.** FUSE needs `/dev/fuse` and `CAP_SYS_ADMIN` in the
     container that mounts. The privileged default has both. The
     `plex.unprivileged` path already adds `SYS_ADMIN` for the network
     namespace and must also pass `/dev/fuse` through (a device plugin or
     a `hostPath` device), documented beside the sysctl requirement.
   - **Same container, same mount namespace.** The manager is PID 1 of the
     Plex container, so the mount is visible to Plex with no mount
     propagation, `Bidirectional` or host involvement. A sidecar would
     have needed all three.
   - **Peers.** The groupcache pool listens on a new container port and
     its peer set is the A records of `plex-workers`, re-resolved on an
     interval and applied with `pool.Set`; a pod leaves the ring when its
     endpoint is gone and its keys reload on demand. The self address is
     the pod's own DNS name under the headless Service.
   - **Probes.** `startupz` holds until the three mounts answer a `stat`
     and the index has loaded; `livez` fails when a mount stops
     answering, since a dead FUSE server leaves Plex with `ENOTCONN` on
     every poster and the pod must be replaced, not left running.
   - **Drain.** `Supervisor.Stop` drains clients, stops Plex, then the
     manager flushes open writers and unmounts. Unmounting under a
     running Plex is never done.
   - **Resources.** The cache budget is a value (`assets.cacheBytes`,
     default 512 MiB) and the chart adds it to the container's memory
     request and limit, which today is 4 GiB. The spool is the existing
     `plex-transcode` `emptyDir` or its own, sized by a value.
   - **The bucket.** Configured by endpoint, bucket and a Secret with the
     credentials (`assets.s3`), the way `postgres` is today. The chart does
     not run MinIO; a cluster that has one (talos runs one for the
     observability stack) points at it, and the chart's NOTES say so. A
     JetStream object store was considered, since clustarr already runs
     NATS with one for artwork, and rejected for now: it would make
     cluster-plex a NATS client for a second reason, and the S3 API is the
     one every cluster can offer.
   - **Seeding and fallback.** A one-shot Job (`assets-seed`) uploads the
     three trees from the existing claim before the first start under
     this ADR; the manager refuses to start Plex against an empty bucket
     unless `assets.allowEmpty` says the library is fresh. If the bucket
     cannot be listed at start, the startup probe fails for its deadline
     rather than starting Plex over an empty tree, because Plex would
     then re-fetch every poster from clustarr's provider.

## Options Considered

### Option A: ReadWriteMany NFS from the NAS

Zero code; the shared claim as ADR-0004 designed it. Every stat and every
read is a network round trip to a disk array whose SSD cache is
write-around, so the Metadata tree's small-file reads hit spinning disks
until cached; and the release depends on the NAS being up for Plex to
show a poster. Still the right answer for the identity files, and the
fallback if this ADR's cost proves too high.

### Option B: this ADR, the cache in the manager

A FUSE hop per stat (microseconds, answered from memory), a node-local hot
cache for the bytes, one fetch per cluster per object, and no NAS in the
read path. The cost is a new package the size of the proxy, a bucket to
run, and a privilege the pod already holds.

### Option C: a sidecar daemon with a shared mount

The same filesystem in its own container. It would need `Bidirectional`
mount propagation on the volume, a privileged sidecar beside a privileged
Plex container, and its own ordering against Plex's start. The manager
already solves ordering, probes and drain for Plex; a second supervisor
for the mount duplicates them.

### Option D: pre-populate Plex's own PhotoTranscoder cache

Write `<sha1>.jpg` entries the proxy predicts into the shared directory,
so Plex sees hits. The hash inputs are undocumented and would move with
any Plex release, and it does nothing for the bundles. Rejected.

## Consequences

**Easier.** Replicas stop depending on a ReadWriteMany class: the three
large trees need only a bucket, and the shared claim shrinks to a few
megabytes. A cold pod serves posters at memory speed after the first
request per key, and a cluster of three fetches each object once.

**Harder.** A FUSE filesystem is a second process in Plex's data path.
Its failure modes are new: a hung getter is a hung `read(2)` in Plex, so
every bucket call carries a deadline and a failed load is `EIO`, never a
block. The assumption that these trees are write-once must hold; a Plex
release that appends to a bundle file in place would need a read-modify-
write path this ADR does not build (the design refuses `O_APPEND` and
partial rewrites with `EOPNOTSUPP` so the change is loud).

**Not covered.** `Preferences.xml`, the plex.tv caches, `Codecs/` and
`Plug-in Support/` stay on a shared claim. The Live TV EPG database stays
per pod in the shim's `emptyDir`.

**To verify in the spike, before the plan.**
- That the Metadata tree really is write-once under a refresh: inotify on
  the volume while one item refreshes, from inside the pod, since the
  earlier probe through `/proc` could not see this directory's events.
- The stat-to-read ratio on a cold Plex start against a 40k-entry index,
  which sets the attribute timeout.
- go-fuse v2 on the arm64 control-plane nodes talos now runs Plex on.

## Action Items

1. [ ] Spike: `pkg/assetfs` mounting one tree read-only over a seeded
       bucket, Plex started against it in kind, posters served.
2. [ ] The write path: spool, upload, rename retargeting, `EIO` on
       failure, with a test that drives Plex's temp-plus-rename sequence.
3. [ ] The peer pool and `/index` fan-out on the manager's HTTP server;
       peer discovery from `plex-workers`.
4. [ ] Wire into `run()` after the network namespace and before
       `runPlex`; unmount after Plex exits; `startupz` and `livez`.
5. [ ] Chart: `assets.*` values, the container port, memory from the cache
       budget, the seed Job, `/dev/fuse` on the unprivileged path, NOTES.
6. [ ] e2e: three pods, one bucket; a poster refreshed on one pod is read
       on the others; a pod deleted mid-read costs nothing; the bucket
       unreachable at start holds the startup probe.
7. [ ] Supersede the `Metadata/`, `Media/` and `Cache/` rows of ADR-0004's
       shared table once this is in.
