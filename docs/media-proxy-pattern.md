# The media proxy pattern

## Why

One Plex Media Server serves every byte, so aggregate throughput is capped by
one node's network interface. Plex itself cannot run as several coordinated
instances, so the only way past that ceiling is to stop routing media bytes
through the server at all.

The bytes a client fetches for direct play are an ordinary HTTP range request
for a file. Any process with the library database and the media can serve them.
That is the whole idea.

## Shape

A dedicated proxy service sits in front of Plex and is the only thing clients
connect to. It is a stateless Deployment, scaled and scheduled independently of
the Plex pods.

    client -> proxy pod  -- media bytes  --> shared media volume
                         -- everything else -> the pod currently running Plex

The proxy asks the leader one small question per media request: is this token
allowed, and which file does this part ID refer to. It then streams the file
itself. Control traffic, metadata, timelines and websockets are forwarded to
the leader untouched.

Plex pods keep running Plex and the transcode workers. They stop being
the data path for playback.

## Coordination

The Kubernetes Lease is the single source of truth for who runs Plex. Nothing
else records it.

The previous design copied that answer onto each pod as a `plex-role` label and
pointed a Service selector at it. That is a cache, and it can disagree with the
Lease: during a crash loop every pod carried `plex-role: leader` at once while
the Lease had one holder. The proxy watches the Lease directly instead, so
there is one answer and it is always current.

Two consequences worth stating:

- Leader election is now only about who runs the Plex process. It no longer
  decides where traffic goes.
- The Plex pods no longer need permission to patch pods.

**Availability, not just election.** The Lease flips the instant leadership
moves, but Plex needs seconds to bind its port. Routing on holder identity
alone sends clients to a server that is not listening. The signal the proxy
follows is "this pod holds the Lease *and* its Plex is accepting connections",
published once the port is confirmed up. Across a transition the proxy holds
and retries rather than failing, which turns a failover into a pause.

A direct-play stream does not touch the leader once it has been authorized, so
playback survives a Plex restart. Today a restart drops every stream.

## Prior art

[UnicornLoadBalancer](https://github.com/UnicornTranscoder/UnicornLoadBalancer)
already does the load-bearing part of this. It routes `/library/parts/:id1/:id2/file.*`
away from Plex, looks the part up in Plex's SQLite database and serves the file
itself from shared storage. That is the same mechanism proposed here, which is
worth knowing before building it.

Its most useful decision for us: it does **not** synthesize timeline reports. It
forwards the client's own `/:/timeline` to Plex untouched and merely observes it
to track session lifecycle. Do the same. Plex accepts timeline posts from third
parties, so synthesizing them is possible, but the client is already sending
them and duplicates risk corrupting watch state.

ClusterPlex, Plex-Remote-Transcoder and rffmpeg all distribute transcoding only.
None of them move the direct-play byte path.

## What this costs

Plex stops seeing the byte flow, and there is no API to tell it otherwise.
`/statistics/bandwidth` is read-only, and Plex's own documentation says the
dashboard graph "only measures data that actually leaves the server device", so
proxy-served bytes are invisible there by definition. `/:/timeline` does accept
a client-estimated `bandwidth` in kbps, but no endpoint anywhere accepts a
transferred-byte count.

Less of this matters than it first appears. A session's `bandwidth` attribute
is a reservation the streaming brain computes from the file's bitrate, not a
measurement of bytes served, so per-session bandwidth never depended on Plex
watching the flow. Only the dashboard graph does. The proxies count bytes per
session themselves, which is more accurate than what Plex had, just not inside
Plex.

**Client addresses.** Plex honours `X-Forwarded-For` only when the forwarded
address is public; private addresses are ignored and the client shows up as the
proxy. There is no trusted-proxy setting. So remote clients keep their real
address and their limits, while in-cluster clients collapse to the proxy's pod
address and are classified local. Local playback is exempt from the Plex Pass
bandwidth limits, so those stop applying to them. Forward a single-entry
header, never a chain.

Mirroring the bytes back to the leader does not fix this. It would put the
entire aggregate through the leader's interface, which is the constraint being
removed. Batching does not help either: batching reduces packets, and the
constraint is bytes.

Transcoded output is a separate problem and is not covered here. Session state
lives in the leader's memory.

## Making the bandwidth real

The proxy is the easy half. Throughput only materialises if the ingress path
spreads across nodes.

- `externalTrafficPolicy: Local` on the proxy Service. With `Cluster`, traffic
  is forwarded to pods on other nodes and SNAT'd, so it hairpins across the
  links being scaled.
- Spread proxy pods across nodes. Pods do not add bandwidth, nodes and their
  interfaces do. Ten pods on three nodes give three nodes of egress.
- The upstream router must install multiple next hops. Cilium advertises the
  service address from every node running a proxy pod, but a router that keeps
  a single best path sends everything to one node. On FRR-based gear this needs
  `maximum-paths` raised.

ECMP hashes per flow, so one client lands on one node. Aggregate scales; a
single stream does not.

The ceiling is then the smallest of: storage throughput, the summed interfaces
of nodes running proxies, and the switch uplink.

## Two things clients need before any of this works

Neither is optional, and both are easy to overlook because the proxy passes its
own tests without them.

**Plex must advertise the proxy.** Left alone, Plex tells clients its own pod
addresses, which nothing outside the cluster can reach, so clients either fail
to connect or find a route that bypasses the proxy and its offload entirely.
The `customConnections` preference fixes this, and the chart sets it from
`proxy.externalURL`.

**The proxy must offer TLS.** Plex clients prefer a secure connection and some
decline a server without one. Serve your own hostname with your own
certificate rather than intercepting Plex's `plex.direct` name with a different
one, which is known to break native clients. The chart mounts a Secret and the
proxy terminates TLS itself.

## Open questions worth testing

- Whether Plex ever terminates a session it observes no reads for. The timeline
  response carries a `terminationCode`, and nobody appears to have soaked this.
- Whether a session Plex is not serving gets a `Session` element at all, which
  decides how it looks to Tautulli and to our own dashboard.
- Whether the `bandwidth` value a client reports on the timeline is consumed by
  anything.
