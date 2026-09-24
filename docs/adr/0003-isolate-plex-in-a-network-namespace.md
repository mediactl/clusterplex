# ADR-0003: Give Plex its own network namespace instead of redirecting into it

**Status:** Accepted
**Date:** 2026-09-18
**Deciders:** cluster-plex maintainers
**Supersedes:** ADR-0002

## Context

ADR-0002 put the manager's TCP proxy in front of Plex with a nat `PREROUTING`
REDIRECT, and it listed the separate-network-namespace design as an alternative
it was rejecting:

> A separate network namespace for PMS with a veth pair. Isolates Plex fully,
> but needs the same NET_ADMIN capability plus NAT for Plex's outbound traffic
> and IP forwarding inside the pod.

It closed by naming the condition to revisit under: "Revisit when the pod stops
being privileged."

That condition turns out to point the other way. The pod *is* privileged, for
FUSE, and will stay that way as long as LiteFS needs `/dev/fuse` without a
device plugin. So two of the three costs — NET_ADMIN and `ip_forward` — are
already paid, and only the NAT is new.

Meanwhile the REDIRECT kept accumulating consequences:

- Plex binds 32400 *and* 32401 and exits with "Error binding acceptor: Address
  in use" if either is taken, so the proxy had to be pushed out to 32499. That
  trap cost a debugging session once already.
- `plex-main` therefore targets a port with no meaning to anyone reading it.
- The image needs `iptables` and the manager shells out to it.
- A REDIRECT only redirects. Plex is still listening on the pod address, so
  anything in the cluster that talks to `podIP:32400` before the rule is
  installed, or that the rule does not match, reaches Plex directly.

Isolation removes the cause rather than compensating for it. Plex keeps binding
32400 and 32401, in a namespace where nothing else wants them; in the pod
namespace both are free, so the proxy simply binds 32400.

## Decision

The manager provisions a network namespace for Plex at startup and launches
Plex inside it. The namespace holds a veth peer addressed `169.254.1.2/30` with
a default route via `169.254.1.1`, the pod-side end of the same pair. The pod
namespace enables `net.ipv4.ip_forward` and installs one nftables rule that
masquerades traffic sourced from the link, which is what lets Plex reach
plex.tv and cluster DNS.

All of it is netlink: `vishvananda/netlink` and `vishvananda/netns` for the
namespace, links, addresses and routes, and `google/nftables` for the
masquerade. Nothing shells out, and `iptables` leaves the image.

The proxy binds 32400 in the pod namespace and forwards over the link, so
`--proxy-port` and the 32499 default are retired.

Three decisions inside the decision, each of which could reasonably have gone
the other way:

- **The namespace is provisioned on every pod, not on election.** A veth pair
  costs nothing, and a failure surfaces at boot on every replica rather than
  during a failover.
- **It is not bind-mounted to `/run/netns`.** Persisting it would buy
  `ip netns exec` for debugging at the cost of state that outlives its container
  and must be reconciled on restart — the same shape as the stale
  `plexmediaserver.pid` bug. `/proc/<pid>/ns/net` serves debugging without it.
- **Provisioning failure is fatal.** Under ADR-0002 a failed REDIRECT was
  survivable, because the Service still reached the proxy port. Here there is no
  fallback: Plex started outside its namespace binds the proxy's port and dies
  in under a tenth of a second, with the reason only in its own log.

## Consequences

- Easier: no redirect rule, no `iptables`, no second port. Plex is unreachable
  except through the proxy by construction rather than by rule. Plex's
  `-progressurl http://127.0.0.1:32400/...` now genuinely addresses Plex.
- Harder: **Plex advertises the wrong address.** It enumerates its interfaces
  and publishes them to plex.tv, and inside the namespace it sees only `lo` and
  `169.254.1.2`. That is narrower than it sounds — the `podIP:32400` it
  advertised before was already useless outside the cluster, and in-cluster
  callers still land on the proxy — but it makes `customConnections` the
  supported way to advertise a usable address. See `docs/configuration.md`.
- Harder: GDM discovery on UDP 32410-32414 does not cross the veth. Broadcast
  discovery was already dead across pod networking.
- New requirement: the node kernel needs nftables with `nft_masq`. Standard for
  years, and Kubernetes now ships an nftables kube-proxy backend, but it is a
  requirement the REDIRECT did not have.
- IPv4 only. The namespace is not addressed for IPv6, and `plexnet` rejects an
  IPv6 subnet rather than half-configuring one.
- Revisit if the pod ever stops being privileged: creating the namespace needs
  CAP_SYS_ADMIN, which is a strictly larger ask than the REDIRECT's
  CAP_NET_ADMIN.

## Amendment, same day: the premise moved, the decision did not

This was written while the library still lived in LiteFS, and its argument for
accepting a privileged pod was that the cost was already paid: "The pod *is*
privileged, for FUSE."

That is no longer true. The library moved to PostgreSQL, LiteFS is gone and
nothing mounts a filesystem any more, so FUSE no longer justifies anything.

The decision stands regardless, but on its own footing rather than on a
borrowed one: creating a network namespace needs CAP_SYS_ADMIN and wiring it
needs CAP_NET_ADMIN, and the pod is privileged for that reason alone now. The
honest reading is that this ADR made the pod's privilege its own requirement
instead of inheriting it, which is a real cost it should be judged on.

The revisit condition is unchanged and now the only one: if the pod should stop
being privileged, this is what stands in the way.

## Action Items

1. [x] `pkg/plex/net`: namespace, veth, addresses, routes and masquerade over netlink.
2. [x] Launch Plex through it (`Supervisor.StartProcess`), fatal on failure.
3. [x] Bind the proxy to 32400; retire `--proxy-port` and `pkg/portredirect`.
4. [x] Drop `iptables` from the image.
5. [x] Prove it in `test/e2e`: separate namespace, no listener on 32499, egress works.
6. [x] Document `customConnections` as the way to advertise a usable address.
