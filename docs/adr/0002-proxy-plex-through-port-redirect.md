# ADR-0002: Put the manager's TCP proxy in front of Plex with a nat REDIRECT

**Status:** Accepted
**Date:** 2026-09-18
**Deciders:** cluster-plex maintainers

## Context

The manager wants to own the connection path into Plex Media Server: it is the
place to count connections, drain on leadership loss, and later add routing.
The `plex-main` Service therefore targets a proxy port rather than Plex's
port (32400). Two problems followed, and one trap.

First, Plex always binds `0.0.0.0:32400`. Inspection of the PMS binary shows
no environment variable, command-line flag or preference that changes the
listen address or port; the only network preferences (`PreferredNetworkInterface`,
`ManualPortMappingPort`) affect what Plex advertises, not where it listens.
Anything on the pod network could reach Plex directly on 32400 and bypass the
proxy, and Plex advertises `podIP:32400` to plex.tv and to its own child
processes (`-progressurl http://127.0.0.1:32400/...`).

Second, the earlier proxy was an HTTP reverse proxy. Plex serves plain HTTP and
TLS (with its plex.direct certificate) on the same port and clients prefer TLS,
so an HTTP-layer proxy without Plex's certificate breaks secure connections.

Alternatives considered for the bind problem:

- A separate network namespace for PMS with a veth pair. Isolates Plex fully,
  but needs the same NET_ADMIN capability plus NAT for Plex's outbound traffic
  and IP forwarding inside the pod.
- Proxy on a different port only, with `customConnections` pointing at the
  load balancer. Leaves 32400 reachable inside the cluster and does nothing
  for TLS.
- A nat `PREROUTING` REDIRECT of inbound 32400 to the proxy port, the pattern
  service meshes use for sidecars.

The trap: Plex binds 32401 as well as 32400 and exits with "Error binding
acceptor: Address in use" if either is taken. The first proxy port chosen was
32401, and Plex died within 70 ms of every start until a throwaway pod showed
Plex's own log line "Listening on port 32401". The proxy now defaults to
32499, which ClusterPlex has used beside Plex for years.

## Decision

The manager runs a byte-level TCP proxy on the proxy port that dials Plex on
loopback, and before starting Plex it installs
`iptables -t nat -A PREROUTING -p tcp --dport 32400 -j REDIRECT --to-ports 32499`
inside the pod's network namespace. Traffic entering the pod for 32400, whether
from the Service, from a worker's progress callback, or from a client that
followed Plex's own advertisement, lands on the proxy. Connections that
originate inside the pod, such as the proxy's dial to loopback and Plex's own
helpers, traverse OUTPUT rather than PREROUTING and are unaffected.

A REDIRECT failure is logged and does not stop Plex: the Service still reaches
the proxy port directly. The proxy is L4 so TLS and WebSocket upgrades pass
through untouched.

## Consequences

- Easier: one control point for every inbound connection; Plex's advertised
  addresses keep working; worker callbacks reach Plex through the proxy and
  arrive from loopback, which Plex treats as local.
- Harder: the pod needs `CAP_NET_ADMIN` and the image needs `iptables`. The pod
  is already privileged for FUSE, so this adds no new privilege today.
- Revisit when the pod stops being privileged: the redirect would need
  NET_ADMIN explicitly, or the network-namespace approach.

## Action Items

1. [x] Replace the HTTP reverse proxy with `pkg/proxy` (TCP) and start it with Plex.
2. [x] Apply the redirect from the supervisor (`pkg/portredirect`), best effort.
3. [x] Point `plex-main` at the proxy port and add `iptables` to the image.
