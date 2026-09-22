# ADR-0005: Retire the media proxy tier in favour of Gateway API session persistence

**Status:** Proposed
**Date:** 2026-09-22
**Deciders:** cluster-plex maintainers
**Supersedes, in part:** `docs/media-proxy-pattern.md`

## Context

The media proxy exists for one reason, stated in the first line of its own
document:

> One Plex Media Server serves every byte, so aggregate throughput is capped by
> one node's network interface. **Plex itself cannot run as several coordinated
> instances**, so the only way past that ceiling is to stop routing media bytes
> through the server at all.

ADR-0004 falsified the middle clause. Plex now runs on every pod against one
PostgreSQL library, verified with three pods answering under one identity. The
ceiling lifts by replication, which is the thing the byte offload was built to
work around.

That makes the tier's central trick — authorize a media request against Plex,
resolve the part ID to a path, then stream the file from the shared volume
without Plex in the data path — a workaround for a constraint that no longer
binds. It is a lot of machinery to keep for a premise that has expired:
`pkg/mediaproxy`, `pkg/plexroute`, `pkg/hashring` and `cmd/proxy`, plus the
annotation protocol the pods maintain to feed it.

### Two different things are called "the proxy"

Only one of them is in scope here.

- **`pkg/proxy`**, an L4 TCP proxy inside every Plex pod. It holds 32400 in the
  pod namespace because Plex binds that port inside its own network namespace
  (ADR-0003). It is deliberately L4: Plex serves plain HTTP and TLS with its
  `plex.direct` certificate on the same port, so an L7 proxy without that
  certificate breaks secure clients (ADR-0002). **This stays.** Nothing outside
  the pod can replace intra-pod plumbing.
- **`cmd/proxy` with `pkg/mediaproxy`**, the tier clients connect to. L7,
  terminating TLS with our own hostname and certificate. **This is what this
  ADR retires.**

### The tier does not currently work

Two defects, both found while writing this, and both arguing that replacing it
beats repairing it:

- **Its pod selector cannot match.** `plexroute.DefaultPlexSelector` is
  `app.kubernetes.io/component=plex`, which is what `charts/` labels pods with.
  `k8s/base/statefulset.yaml` labels them `app: plex`. Deployed from the
  manifests, the router can never find a Plex pod, and logs
  `no pod is serving Plex; requests will wait` for ever. `cp-proxy` has been
  0/1 for four days saying exactly that.
- **The role label no longer follows the lease.** Every pod reads
  `plex-role: worker`, the lease holder included, because `markReady` updates
  the label only on the first call and ADR-0004 made the first call the one
  that marks this pod a worker. The label is documented as observability only
  and nothing routes on it, so this is cosmetic — but it is wrong, and it is a
  regression from ADR-0004.

## Decision

**Clients reach Plex through a Gateway API `HTTPRoute` with
`sessionPersistence`, and the media proxy tier is deleted.**

Every Plex pod is a backend. Session persistence pins a client to one of them
for the life of its session, which is what the hash ring did. Plex serves its
own bytes again, from whichever pod the client is pinned to, and aggregate
throughput scales with the number of Plex pods rather than with a separate
tier.

What each retired piece is replaced by:

| Retired | Replaced by |
| --- | --- |
| `pkg/hashring` session pinning | `sessionPersistence` on the HTTPRoute |
| `pkg/plexroute` pod discovery | The Service's own endpoints |
| `clusterplex.io/plex-serving` annotation | Pod readiness |
| `pkg/mediaproxy` byte serving | Plex itself, on every pod |
| `cmd/proxy` TLS termination | The Gateway listener |
| `proxy.externalURL` | The Gateway's hostname, still written to `customConnections` |

`customConnections` remains the mechanism that makes any of this reachable:
Plex advertises what it is told to advertise, and it must be told the
Gateway's address. That does not change, only the address does.

### Not `sessionPersistence`. Consistent hashing.

This was tested in kind against Envoy Gateway v1.9.1 and Gateway API v1.6.2
experimental, and the mechanism this ADR was named after turned out to be the
wrong one.

`sessionPersistence` is not affinity on a header the client already sends. Per
GEP-1619 the *gateway* mints the session and the client returns it. Envoy
accepts the route — `Accepted=True` — and then answers with its own value:

    x-plex-client-identifier: MTAuMjQ0LjAuMjMzOjgw     # base64 "10.244.0.233:80"

That is the backend endpoint, not the client's identifier. Twelve requests
carrying one `X-Plex-Client-Identifier` spread 9/2/1 across three pods, because
the value the client sent was never a session Envoy had issued. Plex clients
send their own identifier and will never echo Envoy's — and Envoy *overwrites*
that response header trying, clobbering a header Plex's own protocol uses. The
Cookie form is no better for the same reason and worse for non-browsers.

Consistent hashing asks for no cooperation. It hashes a header the client
already sends, which is exactly what `pkg/hashring` does today. Envoy Gateway
exposes it through its own `BackendTrafficPolicy`:

    loadBalancer:
      type: ConsistentHash
      consistentHash:
        type: Header
        header:
          name: X-Plex-Client-Identifier

Measured on the same three pods, same requests:

| Requests | Result |
| --- | --- |
| No header | spread 5 / 4 / 3 |
| `client-aaaa` × 12 | 12 to one pod |
| `client-bbbb` × 12 | 12 to a different pod |

**This costs portability, and that is the trade to weigh.** `BackendTrafficPolicy`
is `gateway.envoyproxy.io/v1alpha1`, not Gateway API, so the routing stops
being implementation-neutral and moving off Envoy Gateway means rewriting it.
The alternative is keeping a hash ring we maintain ourselves, which is what
the tier already is.

The affinity has to outlive a transcode. Since ADR-0004 the transcode chunks
live on a per-pod `emptyDir`, so a client that bounces to another pod mid-stream
finds none of its chunks and playback stops. `absoluteTimeout` therefore has to
exceed the longest session, not the longest request.

## Consequences

**Easier.** Four packages and a Deployment go away, along with the annotation
protocol that kept them in step with the pods and the class of bug where that
protocol disagrees with reality — which is both of the defects above. Plex is
the data path again, which is the arrangement every Plex client is tested
against.

**Harder.** Session affinity moves from something we implement and can debug to
something the Gateway implementation provides, at whatever fidelity it
provides it. A hash ring that pins on a token we choose is more predictable
than persistence keyed on a header the client may or may not send on every
request.

**Resolved, and it changed the decision.** The blocking question was whether
any implementation offers header-based persistence. Envoy Gateway v1.9.1 does
not, in the Gateway API sense, and neither does the spec mean what this ADR
assumed it meant. Consistent hashing through Envoy Gateway's own
`BackendTrafficPolicy` does the job instead, proved above. What is left to
decide is not feasibility but whether an implementation-specific CRD is an
acceptable price for deleting four packages.

**To measure, not assume.** A Gateway is itself a data path. Today media bytes
never touch a Plex pod; afterwards they cross the Gateway *and* a Plex pod, so
the ceiling moves to the Gateway's own throughput. That is a win only while the
Gateway scales as freely as the proxy Deployment did. If it does not, this
trades a bottleneck for a bottleneck and the offload was worth keeping.

## Action Items

1. [x] Prove the affinity mechanism in kind before anything else. Done, and
       it rejected `sessionPersistence` in favour of consistent hashing; see
       above. Envoy Gateway v1.9.1, Gateway API v1.6.2 experimental, both
       installed in the kind cluster.
2. [ ] Measure throughput through the Gateway against throughput through the
       proxy, with enough clients to saturate one pod's interface.
3. [ ] Only then: the Gateway, the HTTPRoute, and `customConnections` pointed
       at its hostname.
4. [ ] Delete `pkg/mediaproxy`, `pkg/plexroute`, `pkg/hashring`, `cmd/proxy`
       and the `clusterplex.io/plex-serving` annotation.
5. [ ] Rewrite `docs/media-proxy-pattern.md` as historical, the way ADR-0001
       was. It is the only record of why the offload existed and what it cost.
6. [x] Independently of all of the above, fix the defects found here. The role
       label follows the lease again, and `plex-main` no longer selects on it
       at all — every pod runs Plex and readiness already means this pod's Plex
       is answering, so selecting the holder sent every client to one pod. A
       third defect turned up while testing: network-namespace provisioning
       was not idempotent across a container restart, so any restart left a
       pod crash-looping for ever on `create veth pair plex0/plex1: file
       exists`. All three are fixed.
7. [ ] The selector mismatch between `charts/` and `k8s/base` remains, and
       matters only while `pkg/plexroute` does. It disappears with the tier.
