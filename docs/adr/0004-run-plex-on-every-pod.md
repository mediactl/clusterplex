# ADR-0004: Run Plex on every pod, and split its state into shared and per-pod

**Status:** Accepted
**Date:** 2026-09-22
**Deciders:** cluster-plex maintainers

## Context

The library moved to PostgreSQL, which is what made several Plex instances on
one library possible at all: there is no database primary to elect and nothing
to replicate. `plex-mode` already accepts `active`, and `runPlex` already has
the branch that starts Plex on every pod. The default stayed `elected` because
that is the path that had been exercised.

Running the `elected` path in kind exposed two things. The first is a defect:
a pod that loses the lease stops Plex, returns from `runElected` — so it also
stops trying to re-acquire — and never resets `runsPlex`, so its readiness
stays false for ever. The StatefulSet waits on readiness, so a rolling update
never finishes. The code intends otherwise; `elect.go` says "Exit so the
container comes back clean rather than half torn down", but nothing exits. That
exit had been happening by accident, through the health watch noticing Plex
gone and calling `os.Exit(1)`, until that path was correctly narrowed to real
crashes.

The second is the reason this ADR exists. Electing a pod to *run Plex* is the
wrong thing to elect. With the library shared, every pod can serve; what still
needs exactly one owner is the connection to plex.tv, because every pod runs
under one server identity and several publishing at once makes that identity
appear to move between addresses. Leadership should decide that, and nothing
else.

### What multi-instance Plex needs, and what we already have

The generic advice for active-active Plex is four parts filesystem and identity
and two parts scheduling. Measured against this repo:

- **One identity across replicas.** Done. Every pod mounts one `Preferences.xml`
  and `pkg/plex/prefs` merges rather than regenerates it, so `MachineIdentifier`,
  `ProcessedMachineIdentifier` and `PlexOnlineToken` are shared by construction.
- **Media at identical absolute paths.** Done. One `plex-media` claim at the
  same `mountPath` in every pod.
- **Sticky sessions.** Done, and not in the load balancer: `pkg/hashring` and
  the proxy tier pin a session to the pod serving it (ADR-0002, ADR-0003).
- **No split-brain scanning.** Done, and more thoroughly than by designating a
  primary. *Every* Butler task is disabled on *every* pod
  (`pkg/plex/prefs/butler.go`) and maintenance is Kubernetes CronJobs calling
  `/api/v1/maintenance/{task}`. There is no scheduler left in Plex to collide.
- **No GDM, no UPnP, a custom access URL.** Mostly done:
  `PublishServerOnPlexOnlineKey=0`, `ManualPortMappingMode=1` and
  `customConnections` are forced in `pkg/plex/prefs/required.go`. GDM is not
  explicitly disabled.

So the settings half of active-active is already built. What is not built is
the filesystem half — and the usual advice, "put `Plug-in Support` on shared
storage", is exactly backwards here.

### Why sharing `Plug-in Support` would corrupt the library

`Plug-in Support/Databases` holds the shim's **shadow SQLite**, and the shadow
is rebuilt on every start, deliberately: the shim writes DDL to it as it runs
and a kept one drifts from what PostgreSQL holds. Today every pod mounts that
directory from one ReadWriteMany claim. In `elected` mode only one pod runs
Plex, so only one rebuilds it and nobody notices. With every pod running Plex,
three pods rebuild one file while the other two hold it open.

The same claim carries three more things that are per-instance rather than
per-cluster:

- `plexmediaserver.pid`. The supervisor removes it before every start, because
  a stale one stops Plex starting and container PIDs repeat. Shared, pods
  delete each other's, and a live one from another pod can stop a pod starting.
- `Logs/`. Three servers appending to one `Plex Media Server.log`.
- The transcode temp directory. Chunks written by the pod serving a session,
  read back by the same pod; shared storage makes that a network round trip per
  chunk for no benefit.

`k8s/base/pvc.yaml` states the assumption that no longer holds, in as many
words: "Only the leader writes to it."

## Decision

**Every pod runs Plex. The lease decides who talks to plex.tv, and nothing
else.**

Concretely:

1. **Plex's lifecycle stops depending on the lease.** `active` becomes the
   deployed mode and Plex starts unconditionally on every pod. The
   `runElected` path — acquire, start Plex, hold, stop Plex, return — goes.
   The wedged-pod defect goes with it, because there is no longer a transition
   that stops Plex on a pod that carries on running.

2. **Plex's state splits in two.** Shared, on the existing ReadWriteMany claim,
   because it is the cluster's and every pod must see one copy:

   | Path | Why shared |
   | --- | --- |
   | `Preferences.xml` | The server identity and the plex.tv token |
   | `Metadata/` | Posters, art and XML another pod just fetched |
   | `Media/` | Bundles, BIF files |
   | `Cache/` | Expensive to rebuild, safe to read concurrently |

   Per-pod, on an `emptyDir`, because it belongs to one Plex process:

   | Path | Why per-pod |
   | --- | --- |
   | `Plug-in Support/Databases/` | The shadow SQLite, rebuilt every start |
   | `Logs/` | One server's log |
   | Transcode temp | Chunks the serving pod reads back |

   `plexmediaserver.pid` was in this table and has been taken out, during
   implementation rather than after. It cannot be split the way the others can:
   it is one file inside a shared directory, so the only mechanism is a
   `subPath` mount, and a mounted file cannot be removed — `removeStalePIDFile`
   would warn on every start for ever. Shared is also safe, which is the better
   reason: the supervisor removes the file immediately before starting Plex and
   Plex reads it only at startup, so the worst two pods starting together can
   do is delete a file the other has already finished reading.

   `emptyDir` rather than a `volumeClaimTemplate` because every one of these is
   either rebuilt on start or disposable between sessions. Nothing here needs
   to outlive the pod, and claim templates are immutable once created — a cost
   this repo has already paid once.

3. **`Preferences.xml` is merged under a lock.** N pods starting together is N
   read-modify-writes to one file on shared storage. The merge takes an
   exclusive lock on the file for the read-through-write, so the last writer
   merges onto the previous one's result rather than onto a stale read.

4. **The lease keeps one job and gains a name for it.** It owns plex.tv:
   egress open for the holder, dropped for everyone else, which is already what
   `applyEgress` and `holdPlexTVLease` do. If per-pod settings differences ever
   become necessary, that hook lives here — but as of this ADR there are none,
   because the Butler tasks are off everywhere and scheduling is CronJobs.

## Consequences

**Easier.** Readiness stops being a function of leadership, so a rolling update
completes: every pod is serving or it is broken, with no third state where a
pod is up, not leader and not ready. Losing the lease becomes an egress change
rather than a restart. Aggregate throughput scales with replicas rather than
sitting on one pod while the others idle.

**Harder.** Plex is being run in a way its authors did not intend, and the
blast radius of a wrong answer is the library rather than a crash. Anything
that writes to shared state without going through PostgreSQL is now a race we
own: the `Metadata` tree is the obvious one, where two pods fetching art for
the same item write the same paths.

**To revisit.** Whether `Cache/` is genuinely safe shared. It is on the shared
side above because it is expensive to rebuild and Plex treats it as a cache,
but that is reasoning, not evidence; if it produces corruption it moves to the
per-pod table and costs only rebuild time.

**Unknown, and still unknown.** Whether
Plex tolerates N instances under one identity at all beyond the library. The
shim solves the database. It does not solve `Plug-in Support/Preferences/`,
the blobs database, or media-provider state, and none of those has been tested
with concurrent writers. The fixes that made 1.43.4 work were all
single-instance.

## Action Items

1. [x] Split the volumes in `k8s/base/statefulset.yaml`; update the comment in
       `k8s/base/pvc.yaml` that says only the leader writes. The chart's
       StatefulSet got the same split later than the base did.
2. [x] Make the transcode temp directory explicit rather than inherited, so it
       lands on the per-pod mount (`plex-transcode-dir`, forced into
       `TranscoderTempDirectory`).
3. [x] Delete `runElected`; start Plex unconditionally; reduce the lease to
       plex.tv ownership.
4. [x] Lock `Preferences.xml` across the merge, with a test that interleaves
       two merges.
5. [x] Superseded: there is no `plex-mode` any more. Active is the only
       mode, and the chart no longer renders the setting.
6. [x] Disable GDM, the one item from the standard checklist not yet forced
       (`GdmEnabled=0` in `pkg/plex/prefs/required.go`).
7. [x] Run several pods serving one library in kind. Three pods came up 3/3
       with no restarts and no errors in any of their logs; all three answer
       `/identity` with `claimed="1"` and the same
       `machineIdentifier=ae5887ec46d6bfac69927015acffa8941a904368` at
       1.43.4.10903; exactly one holds the lease and the other two log
       "does not hold the plex.tv lease; blocking Plex's own services". The
       volume split was checked by writing a marker rather than inferred: a
       file created in one pod's `Databases` directory is invisible to the
       others, while one at the volume root is visible to all.
8. [x] Covered by `test/e2e`: a file scanned in on one pod is listed by the
       others; a transcode session pinned to one pod keeps serving while the
       other two pods are deleted and replaced; every pod refreshing one
       item at once leaves every bundle parseable, and a pod without the
       lease reaches the metadata provider; a deleted pod drains the stream
       it holds to the end; and the lease moves without the identity
       changing. The suite generates media on the host when the volume has
       none.
