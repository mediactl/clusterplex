# Configuring the manager

The manager reads its settings from three places. Later sources win:

1. `/etc/clusterplex/config.yaml`, or the file named by `--config`
2. environment variables
3. command-line flags

Run `manager --help` for the full list of flags. Every flag has an environment
variable: replace the dashes with underscores and prefix it with
`CLUSTERPLEX_`, so `--probe-port` reads `CLUSTERPLEX_PROBE_PORT`. The two
exceptions are `--pod-name` and `--pod-namespace`, which read the unprefixed
`POD_NAME` and `POD_NAMESPACE` that the downward API sets.

A small example:

```yaml
probe-port: 8080
plex-dir: /var/lib/plexmediaserver/Library/Application Support/Plex Media Server

plex:
  preferences:
    - name: FriendlyName
      value: Cluster Plex
```

## Plex preferences

Plex keeps its settings as attributes of a single element in `Preferences.xml`.
The manager writes the preferences you declare into that file before it starts
Plex on the leader, because Plex reads the file once at startup.

Declare them in the config file as a list of name and value pairs:

```yaml
plex:
  preferences:
    - name: FriendlyName
      value: Cluster Plex
    - name: LogVerbose
      value: "0"
```

It is a list rather than a map because the config loader lowercases nested map
keys, which would turn `FriendlyName` into `friendlyname`. Plex preference
names are case sensitive, so the setting would be silently lost.

The same preferences can come from the environment or the command line. The
text after the environment prefix is the preference name, used exactly as
written:

```bash
CLUSTERPLEX_PLEX_PREFERENCE_FriendlyName="Cluster Plex"
manager --plex-preference FriendlyName="Cluster Plex" --plex-preference LogVerbose=0
```

### What the manager does and does not touch

Preferences you declare are enforced on every start. If someone changes one of
them in the Plex user interface, the next restart puts your value back. That is
the point: the declared set is the desired state.

Every attribute you do not declare is carried across untouched. This matters
more than it sounds. The live file holds the server identity and the plex.tv
authentication token, none of which the manager could regenerate. The file is
merged, never rewritten from scratch, and it is written atomically so a crash
partway through cannot leave Plex with half a configuration file.

When nothing has changed, the manager does not write the file at all.

### The server identity

Plex identifies itself to clients and to plex.tv with an ID it derives from
`MachineIdentifier`. Every pod mounts the same Plex state volume, so the
identity is already the same everywhere and already survives a failover. What
pinning adds is reproducibility: rebuild the cluster, or lose the volume, and
clients still see the server they know rather than a new one.

Pin it with a UUID:

```yaml
plex:
  machine-identifier: 9c67996e-8b08-44b9-9c83-a6d317322a2d
```

or `--plex-machine-identifier`, or `CLUSTERPLEX_PLEX_MACHINE_IDENTIFIER`.

Setting this on a server that already has an identity **changes** it. Clients
see a new server and the plex.tv claim is lost. To adopt the identity you
already have rather than replace it, read it from the running leader first:

```bash
kubectl exec -n media <leader> -- sh -c \
  'grep -o "MachineIdentifier=\"[^\"]*\"" \
   "/var/lib/plexmediaserver/Library/Application Support/Plex Media Server/Preferences.xml" | head -1'
```

`ProcessedMachineIdentifier` is the value clients actually see, and it is
rejected if you try to set it. Plex derives it from `MachineIdentifier`
deterministically but with a salt that cannot be reproduced outside Plex, so
the only way to get a correct one is to let Plex generate it. Plex also never
recomputes it once written, so whenever the manager changes
`MachineIdentifier` it deletes the derived value, and Plex regenerates a
matching one on the next start.

### Settings the manager refuses

Besides `ProcessedMachineIdentifier` above, names that are not valid XML
attribute names are rejected, as is a `MachineIdentifier` that is not a UUID.
All of these fail at startup rather than at the first leader election, so a
typo never reaches Plex.

### Settings the architecture fixes

Some settings have exactly one correct value here, and the wrong one fails as
something else entirely — as mysterious load, or as remote access that simply
does not work. Those are written by the manager on every start and **refused as
configuration**: declaring one fails startup with a message naming what to set
instead. They are not defaults to be overridden.

| Setting | Value | Why it is not yours to choose |
| --- | --- | --- |
| `ButlerTask*` (11 of them) | `0` | Each Plex process runs its own copy of the scheduler with no knowledge of the others, so every pod would analyse the same media and hit the same rate-limited providers at once. The work is scheduled as Kubernetes CronJobs instead and handed to one pod per library. |
| `PublishServerOnPlexOnlineKey` | `0` | Every pod runs under one server identity. If each published itself, the last to check in would own the plex.tv record and clients would be handed a pod that only sometimes answers. |
| `ManualPortMappingMode` | `1` | Disables UPnP and NAT-PMP. Otherwise Plex asks the router to forward a port straight to a pod address, routing around the proxy and the load balancer together. |
| `FSEventLibraryUpdatesEnabled`, `FSEventLibraryPartialScanEnabled`, `ScheduledLibraryUpdatesEnabled` | `0` | The two ways Plex starts a scan nobody asked for. Every pod mounts the same media on the same paths, so each would watch the same tree, wake on the same event and scan it into the same library at once — and Plex's scanner assumes it is the only one running. The `refresh` maintenance task replaces them, fanned out to one pod per library. |
| `allowedNetworks` | *refused, not set* | It grants access **without authentication** by client source address, and Plex never sees one here: it runs in its own network namespace behind the in-pod L4 proxy, so every request arrives from the pod end of the veth. A range covering the link drops authentication for everyone who reaches the proxy; any other range matches nothing. |
| `customConnections` | `plex-external-url` | It has to match the address the proxy actually serves, which only that setting knows. |

`allowedNetworks` is the one entry that is refused without the manager writing
a value in its place. The others have a correct value here; that one has none,
so picking one would be a behaviour change nobody asked for.

The Butler block covers analysis, thumbnails and metadata refresh, but none of
those discovers files — `ButlerTaskRefreshLocalMedia` refreshes items Plex
already knows about. Scanning has its own two switches, which is why they are
listed separately. Both default off, so forcing them closes a door rather than
changing behaviour.

Turning one of these back on in the Plex web interface does not survive a
restart, which is deliberate: a single pod quietly re-enabling its own scheduler
would show up as unexplained load rather than as an error.

Remote-access publishing being off does not stop remote clients connecting.
plex.tv still hands out `customConnections`; what stops is Plex probing its own
public address and trying to map a port for it.

### Setting the friendly name

Worth calling out, because it is specific to running Plex this way. With no
`FriendlyName` set, Plex names the server after its hostname, which in a
StatefulSet is the pod name. The displayed name would then change every time
leadership moved. The shipped ConfigMap sets it for that reason.

### Advertising an address clients can use

Plex runs in a network namespace of its own ([ADR-0003](adr/0003-isolate-plex-in-a-network-namespace.md)),
where the only addresses it can see are `lo` and its end of the link to the pod,
`169.254.1.2`. Plex enumerates its interfaces and publishes what it finds to
plex.tv, so that link-local address is what it advertises, and no client can
reach it.

This changes less than it sounds. Before the namespace, Plex advertised
`podIP:32400`, which is equally unusable from outside the cluster; anything
in-cluster that follows the advertisement still lands on the manager's proxy,
which holds 32400 in the pod namespace. What it does mean is that Plex's own
advertisement is never the answer for external access, so set the address
explicitly:

```yaml
plex-external-url: https://plex.example.com:443
```

or, in the chart, `proxy.externalURL`. The manager writes it into
`Preferences.xml` as `customConnections` on every start. It is a setting of its
own rather than a preference because it has to agree with what the proxy
actually serves; declaring `customConnections` directly is refused. Plex treats
it as an additional connection rather than a replacement, so it is additive and
safe to set.

Use the address clients actually reach — the LoadBalancer in front of the proxy,
or whatever ingress sits in front of that.

**Usually you should not set it at all.** Left empty, the manager reads the
LoadBalancer address of the Service named by `plex-external-service`
(`plex-main` by default) before every Plex start, and advertises that:

```yaml
plex-external-service: plex-main
```

Set `plex-external-url` only for an address the cluster cannot see for itself —
a DNS name, an ingress, a certificate — because then that name is the address
rather than whatever the Service happens to hold. An explicit value always wins.

**A written-down address has to be live, not merely well-formed.** Startup checks
the scheme and the host, which a stale address passes. Plex publishes it to
plex.tv, and a client that signs in stops using the address it was given and
switches to the published one — so a stale value works until someone logs in and
then fails on the way back, which looks like the sign-in breaking rather than the
address being wrong. That is the failure reading it from the Service avoids.
Check what plex.tv is handing out:

```bash
curl -s -H "Accept: application/json" -H "X-Plex-Token: $TOKEN" \
  -H "X-Plex-Client-Identifier: diag" \
  "https://plex.tv/api/v2/resources" | jq '.[] | select(.owned) |
    {name, connections: [.connections[] | .uri]}'
```

Note also that plex.tv may publish this as an
`https://<address>.<cert-uuid>.plex.direct:<port>` URI rather than the scheme
written here — an `http://` value on this cluster was handed back as `https`
under a `plex.direct` name. The certificate for that name lives on the Plex pod,
so the address has to reach something that lets Plex terminate its own TLS: an
L4 path. An L7 proxy holding a different certificate, or a plain HTTP listener,
fails the handshake for every client that follows the published connection. This
is the same constraint [ADR-0002](adr/0002-proxy-plex-through-port-redirect.md)
records for the proxy tier, and it applies to anything put in front of Plex.

### Sessions have to stay on one pod

`plex-main` sets `sessionAffinity: ClientIP` with `externalTrafficPolicy: Local`,
and both halves are load-bearing. A playback session is many requests, and since
[ADR-0004](adr/0004-run-plex-on-every-pod.md) the transcode chunks are written to
a per-pod `emptyDir` — so a client spread across pods asks a pod for a chunk it
never produced, and playback stops with `Error code: s1001 (Network)` seconds
in. `Local` is what keeps the affinity meaningful: under the default `Cluster`
policy kube-proxy replaces the source address with a node's, so every external
client hashes the same and lands on one pod.

### A stopping pod lets its streams finish

A pod that is terminating has already left `plex-main`, so no new client
arrives there. The streams it holds are another matter: a session's transcode
chunks live only on that pod, so a client mid-stream cannot pick up where it
was on another one. The manager therefore drains before it stops Plex: it
waits, for up to `drain-timeout` (`CLUSTERPLEX_DRAIN_TIMEOUT`, default `2m`),
for the streams its proxy holds to finish on their own, and only then sends
Plex SIGTERM. A stream is a connection that moved a byte towards the client
within the last fifteen seconds, counting bytes still leaving the kernel's
send queue; a web app's notification socket, open for hours and silent, is
not one, and is closed with Plex rather than waited for -- while it was
waited for, that client was still talking to a pod about to go. Restarting a
Plex that has stopped answering never drains.

The StatefulSet's `terminationGracePeriodSeconds` (180) has to cover the
drain plus the 30 seconds Plex gets to flush; shorter, and the kubelet kills
the pod mid-drain. A `PodDisruptionBudget` holds evictions to one pod at a
time for the same reason: each pod that goes takes its sessions with it.

What a drain cannot do is keep a client on the pod. A client that opens a new
connection after the pod started terminating lands elsewhere, and a transcode
it was watching starts again there from its position; a direct-play stream
carries on unchanged.

### What each pod reports

The manager serves Prometheus metrics on the probe port, `/metrics`:

| Series | Meaning |
| --- | --- |
| `clusterplex_plex_serving` | 1 while this pod's Plex answers for its library, 0 while it is withdrawn |
| `clusterplex_plex_sessions` | Items this pod's Plex is playing to clients, read from `/status/sessions` on every health check |
| `clusterplex_plex_transcode_sessions` | Of those, the ones it is transcoding |
| `clusterplex_proxy_connections` | Client connections open through this pod's proxy; what a drain waits on |
| `clusterplex_manager_is_leader` | 1 on the pod that holds the plex.tv lease; every pod serves regardless |
| `clusterplex_active_jobs`, `clusterplex_jobs_routed_total` | Helper processes running here, and intercepted helper invocations by binary and where they ran |

Three pods splitting one library are invisible without these: nothing else
says which pod holds how many streams.

### Settings that are per-pod, not per-cluster

Rate limits apply to one Plex process, and every pod runs one. `WanTotalMaxUploadRate`
set to 2000000 across three replicas lets the cluster serve three times that.
Divide by the replica count to get the cap that was intended.

`DatabaseCacheSize` sizes SQLite's page cache. The library is PostgreSQL now, so
it reaches only the per-pod shadow database the shim rebuilds on every start;
tune PostgreSQL instead.

The local admin token is per process, and its file is not. Plex writes a fresh
`.LocalAdminToken` on every start into its state directory, which is on the
shared claim, so with three pods starting together the file holds whichever
Plex started last and the other two refuse it with 401. The manager therefore
authenticates with the server's own `PlexOnlineToken` from `Preferences.xml`,
one value for every pod, and falls back to the local admin token only on a
server that has not been claimed. Anything else that calls a pod's API with
the file's token should expect to be refused by two pods in three.

Note that GDM discovery (UDP 32410-32414) does not cross the link either.
Broadcast discovery already did not work across pod networking, so nothing that
worked before stops working.

### Changing the link subnet

`--plex-subnet` sets the point-to-point link joining the pod to Plex's
namespace, and defaults to `169.254.1.0/30`. Nothing outside the pod ever sees
it, so the only reason to change it is a collision with a route the pod already
has:

```yaml
plex-subnet: 10.255.0.0/30
```

It must be IPv4 and hold at least two addresses, so `/30` or wider. The first
usable address is the pod side, the second is Plex.

## Applying a change

The manager reads its configuration once, at startup. Editing the ConfigMap
updates the mounted file but does not affect a running manager. Roll the
StatefulSet to pick up a change:

```bash
kubectl rollout restart statefulset/plex -n media
```
