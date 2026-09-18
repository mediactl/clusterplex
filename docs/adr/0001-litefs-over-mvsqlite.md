# ADR-0001: Keep LiteFS for Plex's databases; do not adopt mvsqlite

**Status:** Accepted
**Date:** 2026-09-18
**Deciders:** cluster-plex maintainers

## Context

cluster-plex runs Plex Media Server (PMS) in a Kubernetes StatefulSet. PMS keeps
its library in SQLite databases, which it opens in WAL mode through the
`libsqlite3.so` it ships (3.53.3, dynamically linked). Today a forked LiteFS,
embedded in the manager, mounts FUSE over the Databases directory, replicates
transactions to the other pods, and elects one primary through a Kubernetes
Lease. Only the primary runs PMS; the others hold a read-only copy and wait.
That gives fast failover but not horizontal scale: one PMS serves all clients.

The question was whether mvsqlite (github.com/losfair/mvsqlite), a multi-writer
MVCC SQLite on FoundationDB, could replace LiteFS so that every pod runs PMS
against one shared database. Two integration paths exist: `LD_PRELOAD` of its
VFS into the application, and `mvsqlite-fuse`, which exposes databases as files.

Facts established while evaluating (verified against the mvsqlite source and
the Plex binaries in our image on 2026-09-18):

- mvsqlite is actively maintained (v0.3.22, April 2026; Apache-2.0) and needs a
  FoundationDB 7.3 cluster plus its `mvstore` server.
- The preload path is mechanically possible: PMS links SQLite dynamically and
  uses 4 KiB pages, which mvsqlite supports.
- mvsqlite does not support WAL. Its VFS exposes no shared-memory hooks, so
  PMS's `PRAGMA journal_mode=WAL` silently leaves the database in rollback
  mode, and writing a page 1 that carries the WAL flag panics the process. An
  existing Plex database would have to be converted before import.
- Writes conflict at COMMIT and surface as `SQLITE_BUSY`; only autocommit
  statements are retried by the preload shim. PMS retries around lock
  acquisition, not commit, so conflicting scanner transactions would be lost.
- mvsqlite's write path asserts exactly one page per write. PMS issued
  multi-page database writes under LiteFS (that is the first of our two LiteFS
  patches). Under mvsqlite the same behaviour would abort PMS instead of being
  patchable.
- `mvsqlite-fuse` is a 650-line experimental frontend with no README. It only
  resolves file names shaped `ns<N>-<M>.db` and `.db-journal`, returns ENOENT
  for `-wal` and `-shm`, and asserts on blocking locks. PMS's fixed file names
  would not resolve, and PMS's own SQLite would then try to enable WAL and hit
  the panic above.
- Independently of the storage engine, PMS assumes it is the only process
  owning its state: every instance would run Butler tasks and library scans
  concurrently, in-process metadata caches would go stale across instances,
  playback and transcode sessions live in one process's memory, and one server
  identity (Preferences.xml, plex.tv token) cannot be published from several
  instances.

## Decision

Keep LiteFS as the replication layer for Plex's SQLite databases, with one PMS
writer elected through the Kubernetes Lease. Obtain horizontal scale where PMS
actually spends CPU, by running transcodes on the other pods through the shim
and the manager's job dispatcher, rather than by running several PMS instances.

Track the LiteFS fork as patches on the upstream tag (`hack/litefs`) instead of
an untracked nested clone, so the dependency is reproducible.

## Options Considered

### Option A: LiteFS, single writer, remote transcode workers

| Dimension | Assessment |
|-----------|------------|
| Complexity | Low: existing code, two small upstream patches |
| Cost | Per-pod PVC for LiteFS data; shared RWX volume for the rest of Plex's state |
| Scalability | Transcodes scale with worker pods; UI and metadata do not |
| Team familiarity | High |

**Pros:** proven model (ClusterPlex uses it); failover in seconds; no new
stateful system. **Cons:** one PMS remains the ceiling for UI and metadata
throughput.

### Option B: mvsqlite through LD_PRELOAD

| Dimension | Assessment |
|-----------|------------|
| Complexity | High: Rust shim inside PMS, database conversion, FoundationDB operator |
| Cost | A FoundationDB cluster and mvstore to run and upgrade |
| Scalability | Multi-writer storage, but PMS itself stays single-instance |
| Team familiarity | Low |

**Pros:** shared, MVCC-consistent database. **Cons:** no WAL; commit-time
conflicts PMS cannot retry; one-page write assertion; does not remove the
application-level single-instance assumptions.

### Option C: mvsqlite-fuse

Not viable: file names would not resolve, WAL files are unhandled, and the
frontend is experimental.

## Trade-off Analysis

Option B pays for a second distributed system and accepts silent behaviour
changes in PMS (rollback journal, discarded commits) without buying the goal:
PMS still cannot run as several coordinated instances. Option A keeps the
database layer boring and moves the scale-out to the process that is both
stateless and CPU-bound.

## Consequences

- Easier: reproducible builds (the fork is patches on a tag); durable state
  (per-pod PVC for LiteFS, shared volume for Plex's identity and bundles);
  workers do real work.
- Harder: UI and metadata throughput remain bounded by one PMS; the shared
  volume must be ReadWriteMany in production.
- Revisit if Plex ever ships a multi-instance mode, or if the transcode path
  stops being the bottleneck.

## Action Items

1. [x] Capture the LiteFS changes as `hack/litefs/*.patch` and fetch upstream at build time.
2. [x] Add a per-pod PVC for `/var/lib/litefs` and a shared claim for Plex's state.
3. [x] Implement the job listeners and dispatcher (`pkg/remoteexec`).
4. [ ] Offer the two LiteFS patches upstream.
5. [ ] Teach workers to run the EasyAudioEncoder helper so those transcodes can also leave the leader.
