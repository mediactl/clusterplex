# Isolate Plex in its own network namespace

**Date:** 2026-09-18
**Status:** Implemented on branch `plex-netns`; e2e scenario written but not yet run
**Supersedes in practice:** ADR-0002 (a new ADR-0003 records the reversal)

## Why

Plex Media Server always binds `0.0.0.0:32400`, and `0.0.0.0:32401` beside it.
It has no setting for its listen address or port. Everything awkward about the
current network path follows from that one fact:

- The manager's TCP proxy cannot use 32400, so it sits on 32499 and the
  `plex-main` Service targets a port that means nothing to anyone.
- Anything on the pod network can reach Plex directly on 32400 and bypass the
  proxy entirely, so a nat `PREROUTING` REDIRECT has to claw that traffic back.
- The pod needs `iptables` in the image and the manager shells out to it.

Give Plex a network namespace of its own and the constraint dissolves. Plex
keeps binding 32400 and 32401, but in a namespace where nothing else wants
them. In the pod namespace both ports are free, so the proxy binds 32400
itself, the REDIRECT becomes unnecessary, and `--proxy-port` has nothing left
to configure.

ADR-0002 considered this and rejected it, for good reasons at the time: it
"needs the same NET_ADMIN capability plus NAT for Plex's outbound traffic and
IP forwarding inside the pod", and it closed by naming the condition to revisit
under. Two of those three costs are already paid — the pod is privileged for
FUSE, so NET_ADMIN and `ip_forward` are free today. Only the NAT is genuinely
new, and it is one masquerade rule.

## Shape

```
        pod netns                            plex netns
  ┌──────────────────────┐              ┌────────────────────┐
  │ proxy        :32400  │──── plex0 ───│ Plex        :32400 │
  │ manager, LiteFS      │ .1.1    .1.2 │ Plex        :32401 │
  │ gRPC         :50051  │              │ lo (localhost)     │
  │ probes       :8080   │              └────────────────────┘
  │ eth0      (pod IP)   │
  └──────────┬───────────┘
             │  net.ipv4.ip_forward = 1
             │  nft: ip saddr 169.254.1.0/30 oifname != "plex0" masquerade
             ▼
     plex.tv, cluster DNS, metadata providers
```

Inbound traffic for 32400 — from the Service, from a worker's progress
callback, or from a client following Plex's own advertisement — arrives in the
pod namespace and is accepted by the proxy, which forwards it over `plex0`.
There is no rule to install and nothing to bypass, because in the pod namespace
Plex is simply not there.

Outbound traffic from Plex leaves with source `169.254.1.2`, which dies the
moment it reaches the pod's `eth0`. The masquerade rewrites it to the pod IP.
Cluster DNS works by the same path: `/etc/resolv.conf` is a mount-namespace
concern and is shared, and the ClusterIP is reached through the default route,
the masquerade, and then kube-proxy in the root namespace as usual.

Two things that look like they should break do not, and both were checked
rather than assumed:

- **The shim still reaches the manager.** It dials a unix socket
  (`cmd/shim/main.go`), not a TCP port, and a unix socket is a filesystem
  object. Plex's helper processes inherit the namespace but keep the mount
  namespace, so nothing changes for them.
- **LiteFS is unaffected.** FUSE is a mount-namespace concern.

One thing gets *better*: Plex tells its children to report progress at
`http://127.0.0.1:32400/...`. Inside its own namespace that loopback address is
Plex itself, which is exactly what Plex meant. Today it is only true by
accident.

## `pkg/plex/net`

One package replaces `pkg/portredirect`. Its whole surface:

```go
func Provision(ctx context.Context, cfg Config, log *slog.Logger) (*Network, error)

func (n *Network) Do(fn func() error) error
func (n *Network) StartProcess(cmd *exec.Cmd) error
func (n *Network) PlexAddr() netip.AddrPort
func (n *Network) Close() error
```

`Config` carries the interface names, the `/30` and the Plex port, all with
defaults, so none of it is hardcoded in the middle of a function. It does not
name an uplink: the masquerade matches `oifname != "plex0"` rather than a
specific egress interface, so it keeps working on a multi-homed pod and needs
no configuration to stay correct.

### `Do` is the only place that touches the thread's namespace

Provisioning and launching Plex both need to run code inside the namespace.
Doing that means switching the calling OS thread with `setns`, and getting it
wrong contaminates the Go runtime: an unlocked thread left in Plex's namespace
goes back to the scheduler, and the next goroutine to land on it silently
inherits Plex's network. The failure is non-local and would be very hard to
attribute.

So the switch exists exactly once, in `Do`, and it is stricter than the usual
pattern in two ways:

1. It runs `fn` on a **dedicated goroutine** that calls `runtime.LockOSThread`,
   so no caller can forget to lock.
2. It unlocks **only if restoring the original namespace succeeded**. If the
   restore fails, the goroutine returns while still locked, and the Go runtime
   destroys the thread rather than reusing a contaminated one.

A deferred restore alone is not enough, because the failing case is precisely
the one where the restore itself does not work.

`StartProcess` runs `cmd.Start()` inside `Do`. The child inherits the
namespace because `fork` copies the calling thread's namespaces, and
`os/exec` forks from the calling goroutine's thread — which `LockOSThread`
has pinned. That chain is load-bearing and invisible, so it gets a comment in
the code.

### Provisioning steps

In the pod namespace: create the veth pair, address the host side
`169.254.1.1/30`, bring it up, move the peer into the new namespace, set
`net.ipv4.ip_forward=1`, and install the nftables table.

Inside the plex namespace, through `Do`: bring up `lo` (Plex needs localhost),
address the peer `169.254.1.2/30`, bring it up, add a default route via
`169.254.1.1`.

Every step checks its error. The sample code this is based on discards most of
them, which turns a missing link into a nil dereference several lines later.
`Provision` also tears down what it created on any failure, so a half-built
namespace never survives to confuse the next attempt.

### Masquerade

`github.com/google/nftables` speaks netlink directly, like `netlink` itself, so
nothing shells out and the image drops `iptables`. The rules live in a table
named `clusterplex` so they are unambiguously ours, which matters because
deleting a table is how the teardown works.

`oifname != "plex0"` keeps the masquerade off traffic going back toward Plex.
The rule would be almost correct without it, which is the kind of "almost" that
produces a confusing packet capture two months later.

## Lifetime

The namespace is provisioned once, when the manager starts, and held for the
life of the process. A container restart gets a fresh namespace.

**On every pod, not just the leader.** A veth pair costs nothing, it makes
failover one step shorter, and — the actual reason — a provisioning failure
then surfaces loudly at boot, on every replica, instead of during an election.

It is deliberately **not** bind-mounted to `/run/netns/plex`. That would buy
`ip netns exec plex` for debugging at the cost of state that outlives its
container and has to be reconciled on restart. This repo has been bitten by
exactly that shape of bug already: the stale `plexmediaserver.pid` that stops
Plex starting, documented in `CLAUDE.md`. Debugging is served instead by
`/proc/<pid>/net/tcp` and `/proc/<pid>/ns/net`, which need no tools and no
persistent state.

## The failure mode inverts

Today `Supervisor.Redirect` is best-effort. `supervisor.go` logs the failure
and starts Plex anyway, because the Service could still reach the proxy port
directly. That was a sound call for a REDIRECT.

It is the wrong call here, and the change is easy to miss. If provisioning
fails and Plex starts anyway, Plex binds `0.0.0.0:32400` **in the pod
namespace — the port the proxy now holds.** Plex then exits with "Error binding
acceptor: Address in use" within a tenth of a second, with the reason only in
its own log. That is the exact trap ADR-0002 documents from the 32401 episode,
re-entered from a new direction.

So provisioning failure is fatal, and `Redirect` is replaced rather than
retargeted:

```go
// StartProcess, when set, starts cmd in place of cmd.Start(), so Plex can be
// launched inside its network namespace.
StartProcess func(*exec.Cmd) error
```

Defaulting to nil means `cmd.Start()`, which keeps the existing supervisor
tests running against their fake binary untouched.

## What this costs

**Plex advertises the wrong address.** Plex enumerates its interfaces and
publishes them to plex.tv as local connections. Inside the namespace it sees
only `lo` and `169.254.1.2`, so that is what it advertises, and no client can
use it.

This is narrower than it first appears. The address it advertises today,
`podIP:32400`, is already useless outside the cluster, and in-cluster callers
still work because they now land on the proxy. External access already depends
on the LoadBalancer. The fix is `customConnections`, which `pkg/plex/prefs`
already knows how to manage; it is an action item below rather than something
to wave away.

**GDM discovery stops.** Plex's broadcast discovery on UDP 32410-32414 does not
cross the veth. Broadcast discovery was already dead across pod networking.

**nftables must be available in the node kernel.** `nft_masq` has been standard
for years and Kubernetes itself now ships an nftables kube-proxy backend, so
this is low risk, but it is a new requirement and belongs in the ADR.

**IPv6 is out of scope.** The namespace is addressed v4-only. Worth stating so
nobody assumes otherwise.

## Testing

`go test ./...` stays hermetic. Creating a namespace needs `CAP_SYS_ADMIN`, so:

- **Unprivileged unit tests** cover the address math, config defaulting and
  validation, and the constructed nftables rule — the pure functions, following
  the repo's existing habit of pushing tricky logic somewhere it can be tested
  without a cluster.
- **Privileged tests** behind a build tag, matching how `e2e` is already
  handled, provision a real namespace and assert the links, addresses and
  routes. They skip when not root.

### The existing e2e test asserts what is being removed

`test/e2e/e2e_test.go` needs care, not just edits:

- Line 192 asserts the `iptables` PREROUTING rule. It must go, and `iptables`
  will not be in the image to run.
- Line 190 requires 32400, 32499 and 50051 listening on the leader. 32499 is
  retired. **32400 keeps passing, but for the wrong reason** — it finds the
  proxy, not Plex. Left alone, the assertion goes green while testing nothing.

The replacement distinguishes the two namespaces with no new tooling:

1. `readlink /proc/1/ns/net` differs from `readlink /proc/<plexpid>/ns/net`.
   Plex is genuinely somewhere else.
2. `/proc/<plexpid>/net/tcp` shows 32400 listening. Plex is up in its namespace.
3. `/proc/net/tcp` shows 32400 listening (the proxy) and **nothing on 32499**.
4. The Service still serves Plex end to end through the proxy.
5. From inside the namespace, egress resolves a name and opens a connection —
   this is the masquerade and DNS path, the riskiest part of the change and the
   part no unit test can observe.
6. Failover: Plex restarts, the namespace is reused, the proxy recovers.

## Action items

1. [x] `pkg/plex/net`: `Provision`, `Do`, `StartProcess`, `Close`, plus config
       defaulting and teardown-on-failure.
2. [x] Unprivileged unit tests; privileged namespace tests behind a build tag.
3. [x] `Supervisor`: replace `Redirect` with `StartProcess`; provisioning
       failure is fatal.
4. [x] `main.go`: provision at startup on every pod; point the proxy at
       `169.254.1.2:32400` and bind `:32400`.
5. [x] `config.go`: drop `--proxy-port` and `ProxyPort`; add the plexnet
       settings.
6. [x] Delete `pkg/portredirect`.
7. [x] Dockerfile: drop `iptables`.
8. [x] Manifests: `containerPort: 32400`, Service `targetPort`, update the
       securityContext comment.
9. [x] Rewrite the affected e2e assertions and add the egress scenario.
10. [x] ADR-0003; mark ADR-0002 superseded.
11. [x] Set `customConnections` so Plex advertises a usable address; document
        it in `docs/configuration.md`.
12. [x] Update `CLAUDE.md`: the layout table, and the 32401 gotcha, which now
        reads differently.
