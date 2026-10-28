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
kubectl debug -n media <leader> -q -i --image=busybox:1.37-musl --target=plex -- sh -c \
  'grep -o "MachineIdentifier=\"[^\"]*\"" \
   "/proc/1/root/var/lib/plexmediaserver/Library/Application Support/Plex Media Server/Preferences.xml" | head -1'
```

The image has no shell of its own (it is built `FROM scratch`), so this runs
busybox in an ephemeral container beside the `plex` container and reads the
file through that container's root, `/proc/1/root`. An image built with
`make docker-build DOCKER_TARGET=debug` carries busybox itself, and there
`kubectl exec` works as usual.

`ProcessedMachineIdentifier` is the value clients actually see, and it is
rejected if you try to set it. Plex derives it from `MachineIdentifier`
deterministically but with a salt that cannot be reproduced outside Plex, so
the only way to get a correct one is to let Plex generate it. Plex also never
recomputes it once written, so whenever the manager changes
`MachineIdentifier` it deletes the derived value, and Plex regenerates a
matching one on the next start.

### Claiming the server and sharing its token

The server's plex.tv token, `PlexOnlineToken`, lives in the one
`Preferences.xml` every pod mounts, so every pod accepts it. Plex writes it
there when the server is claimed; the manager can do the claiming too, as
Plex's own image does with `PLEX_CLAIM`:

- **Claim or re-claim.** Get a code from <https://plex.tv/claim> (it lasts
  about four minutes and works once), put it in the Secret the StatefulSet
  reads `CLUSTERPLEX_PLEX_CLAIM` from (`plex-claim`, key `claim`, in the
  kustomize install; `plex.claimSecret` in the chart), and restart the
  pods. Before Plex starts, the manager exchanges the code with plex.tv as
  the server's `ProcessedMachineIdentifier` and writes the token into
  `Preferences.xml`. It does so when the file has no token, or -- with a
  token Secret, which records a hash of each code spent -- when the code
  is not the one last spent, so a new code replaces a revoked token. The
  exchange runs under the preferences lock: of pods starting together, one
  spends the code. A refused exchange (an expired code) is logged and Plex
  starts with the token it had. Delete the claim Secret afterwards; a spent
  code is harmless but useless.
- **Share it with clustarr, and hold every replica to it.**
  `plex.clustarr.tokenSecret` names a Secret in `plex.clustarr.namespace`
  holding the server's plex.tv account: `token` (the key clustarr's Plex
  watchlist ImportList reads through its `secretRef`), `username` and
  `email` -- `PlexOnlineToken`, `PlexOnlineUsername` and `PlexOnlineMail`.
  While the Secret is absent, or holds no token, the lease holder writes
  the file's account into it (one pod holds the lease at a time, so the
  first lease holder's), checked every minute. From then on **the Secret is
  the master copy** (since 2026-10-07; before, the file was): every pod
  forces the Secret's account into `Preferences.xml` before Plex starts, so
  every replica starts signed in as the same account, and the lease holder
  puts it back whenever the file drifts -- a sign-in through Plex Web lands
  in the file and is undone within a minute. An empty field is never
  forced: one the Secret lacks is filled from the file. To change the
  account, edit or delete the Secret (deleting it lets the next check take
  the file's), or spend a new claim code, which replaces the token in both
  and lets the new account's username and email fill in from the file. The
  manager may `get` and `update` that one Secret and `create` Secrets
  there; it never lists or watches them. The kustomize clustarr component
  grants the name `plex-token`; another name needs its Role edited to
  match.
- A Plex already running keeps the account it read until it restarts: a
  sign-in through Plex Web changes that one process until its next start,
  when the Secret's account is forced back; a claim through
  `CLUSTERPLEX_PLEX_CLAIM` restarts them all anyway.

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
| `ButlerTask*` (15 of them) | `0` | Each Plex process runs its own copy of the scheduler with no knowledge of the others, so every pod would analyse the same media and hit the same rate-limited providers at once. The work is scheduled as Kubernetes CronJobs instead and handed to one pod per library. |
| `PublishServerOnPlexOnlineKey` | `0` | Every pod runs under one server identity. If each published itself, the last to check in would own the plex.tv record and clients would be handed a pod that only sometimes answers. |
| `ManualPortMappingMode` | `1` | Disables UPnP and NAT-PMP. Otherwise Plex asks the router to forward a port straight to a pod address, routing around the proxy and the load balancer together. |
| `FSEventLibraryUpdatesEnabled`, `FSEventLibraryPartialScanEnabled`, `ScheduledLibraryUpdatesEnabled` | `0` | The two ways Plex starts a scan nobody asked for. Every pod mounts the same media on the same paths, so each would watch the same tree, wake on the same event and scan it into the same library at once — and Plex's scanner assumes it is the only one running. The `refresh` maintenance task replaces them, fanned out to one pod per library. |
| `GenerateIntroMarkerBehavior`, `GenerateCreditsMarkerBehavior`, `GenerateAdMarkerBehavior`, `GenerateBIFBehavior`, `GenerateVADBehavior`, `GenerateChapterThumbBehavior`, `LoudnessAnalysisBehavior`, `MusicAnalysisBehavior` | `never` | Plex is a UI and API: clustarr probes the media and detects intros and credits, and the analysis is seeded (ADR 0006). A behaviour at `scheduled` runs in Plex's maintenance window whatever the `ButlerTask*` switches say — plex-2 ran loudness analysis over 11,922 audio tracks and sonic analysis that way with all of them off (2026-10-01). The provisioner also turns off each provisioned library's own switches (`enableBIFGeneration`, `enableIntroMarkerGeneration`, `enableCreditsMarkerGeneration`, `enableAdMarkerGeneration`, `enableVoiceActivityGeneration`, `enableLoudnessAnalysis`), which a library falls back to if one of these is ever reset. |
| `MarkerSource` | `cloud` | Only online markers, never local detection, so credits detection cannot run on a pod even if its behaviour is turned back on. |
| `TranscoderCanOnlyRemuxVideo` | `0` | Plex may convert video (owner, 2026-10-26). It was `1` from 2026-10-01, since squasharr made the files playable, but Plex for iOS asks Live TV for a video transcode (`directPlay=0&directStream=0`) even on the LAN at original quality, and every such tune was refused with decision 4010. A conversion runs on the CPU of the pod's node, so any client that cannot copy a stream costs a real-time transcode, library playback included. Written on every start, so a value set by hand in the Plex UI does not linger. |
| `RelayEnabled` | `0` | A relayed connection is capped, so Plex would have to transcode the video down to fit it on the pod's CPU: the relay stays off, and a client that cannot reach the server fails to connect. |
| `allowMediaDeletion`, `autoEmptyTrash` | `0` | clustarr owns the files. A Plex user deleting one goes behind its back, and emptying the trash automatically after a scan that met a stalled NFS mount deletes the items whose files only looked missing. The `empty-trash` maintenance task still empties it deliberately. |
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

or, in the chart, `plex.externalURL`. The manager writes it into
`Preferences.xml` as `customConnections` on every start. It is a setting of its
own rather than a preference because it has to agree with what the proxy
actually serves; declaring `customConnections` directly is refused. Plex treats
it as an additional connection rather than a replacement, so it is additive and
safe to set.

Use the address clients actually reach — the LoadBalancer in front of the proxy,
or whatever ingress sits in front of that.

**Usually you should not set it at all.** Left empty, the manager reads the
LoadBalancer address of the Service named by `plex-external-service`
(`plex-main` by default; the chart sets it to its client Service, named after
the release and configured under `service`) before every Plex start, and
advertises that:

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

### A hostname through a Gateway (HTTPRoute)

The chart can also publish Plex on a Gateway API listener, for a browser or
an app typing a hostname: `httpRoute.enabled`, with `httpRoute.parentRefs`
naming the Gateway (name, namespace, and `sectionName` for one listener) and
`httpRoute.hostnames` the names. It routes every path to the client Service's
port 32400, so the listener's own certificate fronts Plex; name the hostname
in `plex.externalURL` (`https://plex.example.com:443`) so Plex advertises it.
The `plex.direct` connection plex.tv publishes still needs the L4 path above:
the Gateway terminates TLS with its own certificate, which that name's
handshake refuses.

`httpRoute.extraRules` adds rules of your own to the route, rendered verbatim
beside the one to Plex; the Gateway prefers the longer path prefix whatever
the order. clustarr's provider names the hostname in its image URLs when its
ui's external URL is Plex's, so its `/art` must reach the ui there, or every
refresh that downloads an item's images clears them:

```yaml
httpRoute:
  extraRules:
    - matches:
        - path: {type: PathPrefix, value: /art}
      backendRefs:
        - name: clustarr-ui
          port: 8080
```

The chart refuses the route with more than one Plex pod. Through a Gateway
every client arrives from the Gateway's own address, so the client Service's
`ClientIP` affinity below pins all of them to one pod, and a session spread
across pods loses its transcode. Several pods are reached through the
LoadBalancer Service.

### Sessions have to stay on one pod

`plex-main` (the chart's client Service, `<fullname>`) sets `sessionAffinity:
ClientIP` with `externalTrafficPolicy: Local`, and both halves are
load-bearing; the chart refuses to render any other policy, and the affinity's
three hours are `service.sessionAffinityTimeoutSeconds`. A playback session is many requests, and since
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
Plex SIGTERM. A stream is a connection that moved at least 32 KiB towards
the client within the last fifteen seconds or so, counting bytes still
leaving the kernel's send queue. A client taking video or audio moves that
in a second; a web app polling its timeline every few seconds moves a few
hundred bytes, and its notification socket none for hours. Those two are
not streams and are closed with Plex rather than waited for -- while they
were waited for, every rollout took the full drain per pod and that client
stayed on a pod about to go, its next playback starting there. Restarting a
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

#### Tautulli's view

The same read of `/status/sessions` is taken apart the way Tautulli takes it
apart (`pkg/plex/activity`), so a dashboard can show what Tautulli would:

| Series | Meaning |
| --- | --- |
| `clusterplex_plex_streams{user,media_type,decision,location,player,platform,state,resolution}` | Playbacks being served. `decision` is Tautulli's: `transcode` when the video or audio is transcoded, `direct_stream` when either is copied into a new container, else `direct_play`. `media_type` is Plex's, or `live` for Live TV; `resolution` is the source's |
| `clusterplex_plex_stream_bandwidth_bytes_per_second{user,location}` | What Plex budgets for those playbacks, LAN and WAN |
| `clusterplex_plex_transcodes{video,audio,subtitle,hw_decode,hw_encode}` | Transcode sessions by decision and by hardware decoder and encoder (`none` is software) |
| `clusterplex_plex_transcodes_throttled` | Transcodes far enough ahead of their player that Plex throttled them |
| `clusterplex_plex_transcodes_lagging` | Transcodes slower than real time and not finished: a player waiting on them |
| `clusterplex_plex_plays_total{user,media_type,decision,player}` | Finished plays. A video play under two minutes is not counted (Tautulli's ignore interval); a track always is |
| `clusterplex_plex_plays_watched_total{user,media_type}` | Finished plays that got at least 85% through the item |
| `clusterplex_plex_watch_seconds_total{user,media_type}` | Time spent playing, paused time excluded |
| `clusterplex_plex_library_items{section,section_type,type}` | Items per library section: movies; shows, seasons and episodes; artists, albums and tracks. Counted every five minutes by the lease holder alone, so the series do not repeat per pod |
| `clusterplex_plex_server_info{version,platform}` | 1, labelled with this pod's Plex version |

Everything but the library is per pod, because each pod's Plex lists only
the sessions it serves: sum across pods for the cluster. A play starts when
a read first lists it and ends when one no longer does, so a play shorter
than the ten-second health check can go unseen, and a client a drain moves
to another pod starts a new play there. Watch time is credited per interval
by what the previous read saw, and at most 30 s per interval, so reads that
failed for a while are not counted as watching. A pod whose Plex has not
answered for 45 s reports no streams rather than the ones it last saw.

The `user` label carries Plex usernames, and `/metrics` is unauthenticated
on the probe port.

Plex sees a client's own address only when the proxy forwards a public one
("Client addresses" in `docs/media-proxy-pattern.md`): a client on a private
address arrives from `169.254.1.1`, the pod end of Plex's veth, and reads as
`lan`, which it is. A client that reports only timelines gets no `Session`
element from Plex, so its bandwidth reads 0.

#### OpenTelemetry

Titles are unbounded as Prometheus labels, so each finished play is also an
OpenTelemetry span, `plex.play`, from the read that first listed it to the
last one that did, each its own trace: `plex.title` (Tautulli's form, "Show -
S01E02 - Episode"), `plex.user`, `plex.player.*`, `plex.decision`,
`plex.transcode.*`, `plex.watch_seconds`, `plex.paused_seconds`,
`plex.progress_percent`, `plex.watched`, `client.address` and more. The
manager logs the same play as `play ended`.

Spans go nowhere unless an OTLP endpoint is configured, through the standard
environment the chart's `otel` values render:

| Value | Variable | |
| --- | --- | --- |
| `otel.endpoint` | `OTEL_EXPORTER_OTLP_ENDPOINT` | An OTLP/gRPC collector, e.g. `http://otel-collector.observability:4317`. Empty exports nothing. gRPC only: `OTEL_EXPORTER_OTLP_PROTOCOL` set to anything else is refused, logged, and leaves export off |
| `otel.insecure` | `OTEL_EXPORTER_OTLP_INSECURE` | Plain-text gRPC (default `true`) |
| `otel.resourceAttributes` | `OTEL_RESOURCE_ATTRIBUTES` | Extra resource attributes; `service.name` (`clusterplex-manager`), `k8s.pod.name` and `k8s.namespace.name` are set already |

With an endpoint, every series of `/metrics` -- these and the ones above --
is pushed as OTLP metrics too, through OpenTelemetry's Prometheus bridge,
every `OTEL_METRIC_EXPORT_INTERVAL` (default 60 s). The other standard
variables apply: `OTEL_TRACES_EXPORTER=none` or `OTEL_METRICS_EXPORTER=none`
turns one signal off, `OTEL_SDK_DISABLED=true` both, and the
`OTEL_EXPORTER_OTLP_TRACES_*` and `_METRICS_*` variants set one signal's
endpoint and options. The remote-execution spans the shim carries
(`ExecuteRemote`) export the same way.

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

### Running without privilege

The pods run privileged by default. Plex's network namespace needs
`CAP_SYS_ADMIN` to create and `CAP_NET_ADMIN` to wire up, and IPv4 forwarding
on in the pod namespace; privilege gives all three, and the render device for
hardware transcoding with it.

The form without privilege is `k8s/components/unprivileged` (chart:
`plex.unprivileged: true`): the two capabilities, and the forwarding set by
Kubernetes through the pod's `securityContext.sysctls` before any container
starts, which the manager then finds already on. The cluster has to provide
two things first:

- `net.ipv4.ip_forward` is an "unsafe" sysctl to Kubernetes, so every kubelet
  must allow-list it (`KubeletConfiguration` `allowedUnsafeSysctls`), or the
  pod is rejected with `SysctlForbidden`. `hack/kind.sh` creates kind
  clusters with it allowed.
- The render device has to come from a device plugin, for example
  `gpu.intel.com/i915` in the container's resources. Mounting `/dev/dri` is
  not enough: Plex finds the device and the device cgroup refuses it, and it
  transcodes in software. Everything else works unprivileged, tried on kind.

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

## Provisioning libraries and following clustarr

The manager can set up Plex's metadata agents and libraries itself, so that
nothing is configured in the Plex UI, and keep a library current with a
[clustarr](https://github.com/mediactl/clustarr) install. Three keys under
`plex:` declare it (chart values of the same names):

```yaml
plex:
  metadataProviders:
    - uri: http://clustarr-ui.clustarr-system.svc:8080/plex/movies
    - uri: http://clustarr-ui.clustarr-system.svc:8080/plex/tv
  libraries:
    - name: Movies
      type: movie            # movie | show
      provider: tv.plex.agents.custom.clustarr.movies
      language: en-US
      locations: [/media/movies]
      switchAgent: false
  clustarr:
    enabled: true
    namespace: clustarr-system
    pathMappings:
      - {clustarr: /data/media, plex: /media}
```

- **Only the Lease holder acts,** so Plex's configuration has one writer at a
  time. It provisions when it wins the Lease and every 10 minutes after,
  backing off from 30 seconds while Plex or a provider is not answering yet.
- **It creates and updates; it never deletes.** A provider is registered by
  its root, one agent is created per provider (clustarr's alone, no Plex
  fallback), and each library is created on its provider's agent. Removing an
  entry leaves it in Plex. Live TV is the one exception, below.
- **An existing library on another agent is only reported** unless
  `switchAgent: true`: `clusterplex_library_agent_drift{library}` reads 1 and
  the manager logs it. With `switchAgent` set, the library is moved onto the
  agent and force-refreshed, since Plex otherwise applies a new agent only to
  items added afterwards.
- **Following clustarr** watches its MediaFiles, Movies, Series and Episodes
  read-only (the chart grants `get`, `list` and `watch` in
  `plex.clustarr.namespace`, plus the token Secret below when one is named). A file that appears, moves or
  goes away rescans its folder, 30 seconds after the folder goes quiet; more
  than 50 folders at once rescan the library instead. An item whose metadata
  changes is refreshed. `pathMappings` translate clustarr's paths to Plex's,
  longest prefix first.
- **Plex has to see clustarr's files.** Set `storage.media.existingClaim`
  (and `subPath` if needed) to a claim **in Plex's namespace** bound to the
  same volume as clustarr's library; the chart then mounts it at `/media`
  instead of creating `<release>-media`. A claim is namespaced, so
  clustarr's own claim in its namespace cannot be named here: create a
  second claim (for NFS, a second PersistentVolume for the same export, or
  one the storage class lets two namespaces share).
- Watch `clusterplex_clustarr_watch_synced`: it reads 0 when the watch has
  not synced within two minutes, which is what missing clustarr CRDs, a
  wrong `plex.clustarr.namespace` or missing RBAC look like.
- **Plex's rows for the provisioned libraries come from clustarr** (ADR
  0008). The lease holder writes each MediaFile's item, folder, part,
  media row, streams and markers straight into the library the moment
  clustarr records it, and updates the same row when a file is renamed
  or transcoded, so Plex shows one version and nothing lands in the
  trash. Plex's scanner never runs for those libraries. "Scan Library
  Files" is a reconcile of Plex's rows against clustarr's records: it
  takes seconds, adds what is missing, removes what clustarr no longer
  has, and leaves alone any file clustarr never imported. The chart
  points the proxy's `scan-route` at the manager's `/api/v1/scan`; a
  client that reaches Plex over TLS still gets Plex's own walk, which
  finds nothing to do. `pathMappings` must cover every clustarr root
  folder, or its files are counted as unmappable and never projected.

| Metric | Meaning |
| --- | --- |
| `clusterplex_provision_runs_total{result}` | Provisioning passes: `converged`, `pending` or `error` |
| `clusterplex_library_agent_drift{library}` | 1 while a configured library is on another agent |
| `clusterplex_clustarr_scans_total{scope}` | Rescans sent, `folder` or `section` |
| `clusterplex_clustarr_refreshes_total` | Item refreshes sent |
| `clusterplex_clustarr_unmappable_paths_total` | clustarr paths no mapping covers |
| `clusterplex_clustarr_uncovered_paths_total` | Mapped paths no library covers |
| `clusterplex_clustarr_watch_synced` | 1 once the clustarr watch has synced |
| `clusterplex_seed_files_total{result}` | Files projected: `seeded`, `unmatched` (the item is not in clustarr's caches yet) or `error` |

## Live TV: clustarr's IPTVProviders as DVRs

With `plex.liveTV.enabled` the lease holder adds each of clustarr's
`IPTVProvider`s in `plex.clustarr.namespace` to Plex as DVRs (clustarr's
Live TV spec 2026-10-07 §6.1).

```yaml
plex:
  clustarr:
    namespace: clustarr-system
  liveTV:
    enabled: true
    language: eng              # the DVRs' guide language
    restartEvery: 10m          # PMS restarts on the other pods, at most this often
    staleGrabAfter: 5m         # a Live TV grab no player watches is ended after this
```

**Blocks.** PMS takes one device's channel map in one request, capped at
32 KiB of request line: about 454 channels (`docs/livetv-pms-calls.md`).
clustarr therefore splits a provider's channels into blocks of contiguous
numbers and publishes them in `status.blocks`. Each block is one emulated
HDHomeRun and one Plex DVR, with its own device ID, path, lineup and slice
of the guide. Its boundaries are sticky, so a channel rarely changes DVR and
keeps its recording rules.

**The address.** Plex registers each block at its `status.blocks[].uri`,
`http://<release>-livetv.<ns>.svc.cluster.local:80/hdhr/<deviceID>` on
clustarr's one `livetv` Service (`status.address`), which its network
namespace reaches as it reaches every cluster Service (ADR-0003). The tuner
tells the blocks apart by the path and builds every URL it hands out from
the `Host` PMS dialled, so every pod reaches every device the same way and
nothing in a Plex pod proxies or listens for Live TV (clustarr's
direct-tuners design, 2026-10-27; until then each pod proxied a link-local
port per block).

**What it does,** for each provider that is Ready and enabled, each block in
order:

1. **The Lease.** It takes, or renews, the Lease `livetv-<device id>` in
   clustarr's namespace, held as the server's `machineIdentifier`: one
   installation registers each block. A block another installation holds is
   reported (`DVRBlockOwned`) and skipped; if this Plex has that block's
   device too, the Event is a Warning, and the DVR is left for you to
   delete in one of the two. clustarr shows the holder as
   `status.blocks[].owner`. It keeps renewing the Lease while this Plex has
   the block's device: while the provider is not Ready or is disabled, and
   while PMS cannot be read.
2. **The device.** It registers the device at the block's URI, once. A
   device PMS already lists at that URI is never registered again:
   registering again is how duplicate and empty DVR rows arise. A device
   PMS lists with the block's device ID at any other URI -- the link-local
   address before 2026-10-27, a renamed release -- is clustarr's own at a
   stale address and is replaced: its DVRs and the device are deleted, then
   the block is registered at its URI and its DVR created
   (`DVRDeviceReplaced`). The DVR's recording rules go with it. Where PMS
   lists a device it just registered under another form of the URI, that
   form is kept as the device's (`DVRDeviceStoredElsewhere`, a Warning) and
   noted on the block's Lease (`livetv.clusterplex.io/registered`), so no
   later pass, and no later holder, replaces it again.
3. **The DVR.**
   - **Under `epgSource: XEPG`:** it creates the DVR over the block's guide
     (`<uri>/xmltv.xml`), a subset of the provider's
     feeds holding only the block's channels.
   - **Under `PMS`:** it records `DVRNeedsLineup` and waits. Finish each
     block's DVR once in Plex's setup wizard, choosing your location's
     lineup.
   - **A device whose DVR this Plex does not list,** though the database
     ties it to one (its `parentID`), gets no second DVR: the lease
     holder's Plex read its DVRs before another pod made that one. It
     records `DVRListStale` and restarts its own PMS, once; a DVR still
     unlisted after that restart is gone, and a new one is created.
4. **The channel map.** It saves the block's whole channel map whenever its
   `lineupHash` changes, in one request: Plex replaces the whole map on each
   save. A map over 30,000 bytes is refused (`DVRChannelMapTooLarge`);
   clustarr's blocks keep under it.
5. **The guide.** It reloads the block's guide whenever its `guideHash`
   changes. A map save rebuilds the guide, so no reload follows one.

What it saved is noted on the block's Lease (annotation
`livetv.clusterplex.io/saved`: the DVR and both hashes), so the next lease
holder, after a rolling update, saves and reloads nothing it need not. A
Lease that changes installation drops the note.

**While PMS loads a guide** (`/activities` lists `provider.epg.load`), it
deletes nothing, saves no map, reloads no guide and creates no DVR: a DVR
deleted during a load deadlocked PMS (recorded). It asks `/activities` again
before each of those, since PMS refreshes guides on its own. The step waits
for a later pass (`DVRWaitingForGuide`), 20 seconds on. Creating a DVR,
saving a map and reloading a guide each start a load, so a pass starts at
most one.

**What it manages:** only HDHomeRun devices carrying a live block's device
ID, or under the `/hdhr/` of a livetv Service a provider has named. Every
other tuner and DVR is left alone.

**The deletion exception:** this is the provisioner's one deletion (the
owner's ruling, 2026-10-07). It deletes:
- the DVR and device of a block no live provider names: a removed block, or
  a deleted provider's. That takes the DVR's recording rules with it;
- duplicate DVR rows on a managed device;
- ghosts: DVRs whose guide is under a livetv Service's `/hdhr/` but which
  list no device PMS has.

`enabled: false` deletes nothing; Plex shows the devices offline.

**The other pods.** Each PMS reads its DVRs once, at its start, and no call
refreshes them (recorded). After a pass that created or deleted a DVR, the
lease holder restarts PMS on each other pod in turn, waiting for each to
answer for its library again, at most once per `restartEvery`
(`DVRPodsRestarted`). A channel-map save reaches the other pods without a
restart.

A pod whose in-place restart fails is handed back to its health watch,
which restarts Plex and, if it keeps failing, the pod.

**Events:** it records `DVRProvisioned`, `DVRChannelMapSaved`,
`DVRNeedsLineup`, `DVRBlockOwned`, `DVRDeviceReplaced`, `DVRDeviceStoredElsewhere`,
`DVRWaitingForGuide`, `DVRChannelMapTooLarge`, `DVRListStale`,
`DVRProvisionFailed`, `DVRRemoved` and `DVRPodsRestarted` as
`events.k8s.io` Events on the provider (`kubectl describe iptv`).
- **RBAC:** in clustarr's namespace the chart grants `get`, `list` and
  `watch` on `iptvproviders.clustarr.io`, `create` and `patch` on
  `events.k8s.io` Events, and `get`, `create` and `update` on
  `coordination.k8s.io` Leases. In Plex's namespace it grants `patch` on
  Pods, for the restart annotations.
- **Before the CRD exists:** the watch logs once and checks again every 5
  minutes. It never holds up the catalog watch.

**The calls** are recorded against the owner's PMS in
`docs/livetv-pms-calls.md`.

### The guide, in PostgreSQL

To PMS each DVR's guide is a database of its own:
`Plug-in Support/Databases/tv.plex.providers.epg.<provider>-<DVR uuid>.db`,
with the library's schema. The shim serves it from the schema
`epg_<uuid without dashes>` in the library's database. Every pod therefore
reads one guide, and a replaced pod starts with it. Before this, each pod's
guide was a SQLite file on its emptyDir. On talos, 2026-10-20, plex-0 read
"Unknown Airing" on every channel after its pod was replaced.

- **Before each PMS start,** the manager makes the schema of each DVR the
  library lists, as a copy of the library schema's structure: its tables,
  sequences, views and triggers, without the spelling-vocabulary triggers.
  It then replaces the guide's file with a shadow, as it does the library's.
- **A guide load never passes through the shim.** PMS copies its bundled
  database to `<guide>-loading.db`, fills it, and renames it onto the
  guide's file. The shim goes by file name, so the load stays in SQLite.
- **The manager copies each load into the schema.** Every 30 seconds each
  pod copies a guide file its PMS changed. The copy fills a staging schema,
  which then takes the guide schema's name, so PMS reads the old guide or
  the new one and never waits.
  - A file holding no airings (the shadow) is not copied.
  - Nor is a file modified before the schema's last copy, such as another
    pod's older load.
  - A load not yet copied when PMS restarts is copied before the shadow
    replaces it.
- **A guide with no airings** is reloaded by the lease holder, at most
  hourly.
- **A DVR created while PMS runs** gets a SQLite guide, because the shim
  finds no schema for it. Its first load is copied into a new schema, and
  this PMS keeps reading the file until it reopens the guide.

### Grabs no player watches

PMS ends a Live TV grab only when its client reports that playback
stopped ("Recorder: No more consumers"). Stopping the client's transcode
doesn't end it. A client that is killed or loses its network mid-play
reports nothing, so its grab keeps one of the device's tuners until PMS
restarts.

On talos, 2026-10-20, one such grab held the only tuner for six hours:
- every tune waited PMS's 300 seconds for a tuner, then failed with
  `Recorder: Error 23 (Tuner in use by another process)`;
- the client showed "The recording conflicts with other recordings";
- each failed tune stayed listed in `/livetv/sessions` and held the tuner
  in turn.

Every pod therefore looks at its own PMS every 30 seconds
(`pkg/plex/livetv.StaleGrabs`):
- A **Live TV grab** is a `grabber.grab` activity whose subtitle begins
  "Live TV - Session". Its `Context.transcode_session` names its live
  session.
- The grab counts as **watched** while `/status/sessions` lists that live
  session, by its key or its item's ratingKey.
- **A grab unwatched for `staleGrabAfter`** (default 5m) has its activity
  cancelled (`DELETE /activities/<uuid>`), which ends the grab and its
  live session.
- **Two other ways to end a grab don't work:** `DELETE
  /livetv/sessions/<id>` answers 404, and a stopped timeline from another
  client is ignored.
- **A DVR recording**, whose subtitle is the programme's title, runs with
  no player by design and is never touched.

The counter is `clusterplex_livetv_stale_grabs_ended_total`.

### The audio decoders a tune needs

Plex's transcoder ships without most audio decoders: AAC, MP2 and MP3 among
them. PMS downloads each one from plex.tv into `Codecs/` on the config
volume, in one folder per transcoder build and platform
(`Codecs/a336ba9-16a9aaf440ee9a92b75f070b-linux-aarch64/`). It does so only
when it starts a transcode of a file it has analysed and found to need one.

PMS never analyses a Live TV stream, so a tune never downloads a decoder.
Without one, every tune of a channel with that audio fails:
- the transcoder logs `[stream_segment,ssegment] sample rate not set` and
  exits 234;
- PMS logs `Recorder: Error 16`;
- the client shows "Could not tune channel. Please check your tuner or
  antenna".

A Codecs folder lacks the decoders when:
- the config volume is new;
- PMS has upgraded into a new folder;
- the pods run on a new architecture.

talos's arm64 Plex was the third case: it had no AAC decoder until
2026-10-20.

**The prefetch.** The lease holder therefore has PMS download them itself
(`pkg/plex/codecs`), once PMS answers for its library. For each codec in
`--plex-codec-prefetch` (default `aac,ac3,eac3,mp2,mp3`, the codecs Live TV
streams carry; `none` turns it off):

1. It takes a movie or episode whose first audio track is that codec and
   whose video is H.264 or HEVC.
2. It asks PMS for an HLS transcode that copies the video and converts the
   audio, as "Plex for Android (TV)" with HEVC appended to its profile. Plex
   Web's profile copies H.264 alone, and a decision that converts the video
   is refused: it would be a real-time transcode that fetches no decoder.
3. It fetches the transcode's first segment, then stops the transcode. The
   transcoder cannot write a segment without the decoder, and only a served
   segment proves anything: PMS's variant playlist lists every segment of
   the file from the start, 715 for a two-hour film.

Under PMS 1.43.4 that profile copies H.264 and HEVC Main 10 beside AAC,
AC-3, E-AC-3, DTS, TrueHD, FLAC and Opus. PMS still refuses to copy a few
files, Hi10P H.264 among them, so up to five items are tried per codec,
each costing one decision.

**E-AC-3** is the exception. PMS decodes it through its EasyAudioEncoder
rather than a downloaded library, and the same transcode proves that
EasyAudioEncoder runs.

**Licensing.** The decoders are Plex's own downloads for this server. Nothing
ships or copies one, which also keeps them matched to the transcoder build
PMS is running.

**What it records.** What it fetched, it records per platform and PMS version
in `Codecs/.clusterplex-prefetch.json`. A later lease holder fetches nothing
again until the version changes, since a new PMS version may use a new
folder.

**Codecs it cannot fetch.**
- A codec that no item carries as its first audio track is left for PMS to
  fetch on first use, as before. MP2 and MP3 rarely appear in a movie
  library.
- A codec whose transcode fails is tried again after 30 seconds, then after
  waits that double up to an hour.

**Where it runs.** Only on the lease holder, because only the lease holder
reaches plex.tv, and never under `--block-plex-tv`. Pods of a second
architecture share the volume but get their decoders only when one of them
holds the Lease.

**Adding codecs.** A codec named here also helps library playback on the
other pods: a decoder they download on demand needs plex.tv, which only the
lease holder reaches. Add `dca` and `truehd` if the library holds them:

```yaml
plex-codec-prefetch: [aac, ac3, eac3, mp2, mp3, dca, truehd]
```

The chart's value is `plex.codecPrefetch`.

## The remux pool

Plex Web's DASH streams run on the remux pool rather than on Plex's
transcoder (ADR-0007). The manager finds the pool's pods by label and dials
their gRPC port:

| Flag | Default | |
| --- | --- | --- |
| `--remux-selector` | `app=plex-remux` | Label selector of the pool's pods. Empty turns the pool off: every browser stream stays on Plex's transcoder. The chart renders it empty when `remux.enabled` is false. |
| `--remux-port` | `50052` | The remux workers' gRPC port. |

With no ready remux pod, or for any job the pool refuses, Plex's transcoder
runs the stream as before.

## The GPU pool

Every video conversion Plex is asked for runs on the GPU pool: an Intel GPU,
encoding with QSV in process (ADR-0010). That covers Live TV and library
playback from any client, downloads, Optimize and the DVR's
convert-while-recording. There is no fallback. A conversion the pool cannot
take is refused and never runs on Plex's node, so the player shows a
playback error.

| Flag | Default | |
| --- | --- | --- |
| `--video-selector` | `app=plex-video-transcode` | Label selector of the pool's pods. Empty turns the pool off, and every video conversion is refused. The chart renders it empty when `videoTranscode.enabled` is false. |
| `--video-port` | `50053` | The pool's gRPC port. |
| `--video-lease-name` | `cluster-plex-video-transcodes` | The Lease, in the manager's namespace, that counts live conversions. clustarr's transcode admission admits nothing to its Intel class while the count is above zero (clustarr's `manager.videoTranscodeLease`, `<namespace>/<name>`). |

The chart's values:

| Value | Default | |
| --- | --- | --- |
| `videoTranscode.enabled` | `false` | Run the pool. |
| `videoTranscode.image` | `ghcr.io/mediactl/cluster-plex-video` | The tag defaults to the chart's `appVersion`. |
| `videoTranscode.replicas` | `1` | One pod per GPU. |
| `videoTranscode.maxJobs` | `3` | Conversions per pod. A whole-file job (a download, Optimize) never takes the last free slot. |
| `videoTranscode.encoder` | `h264_qsv` | `libx264` encodes and scales on the CPU, with no GPU needed. |
| `videoTranscode.renderNode` | `/dev/dri/renderD128` | The GPU's render node in the pod. |
| `videoTranscode.renderGroups` | `[]` | The group that owns the render node, as `supplementalGroups`. Leave it empty where the node is `0666`. |
| `videoTranscode.gpu` | `{resource: gpu.intel.com/i915, count: 1}` | The device plugin's resource, added to the container's limits. |
| `videoTranscode.affinity` | required `intel.feature.node.kubernetes.io/gpu=true` | NFD's Intel rule. `nodeSelector` and `tolerations` are also available. |
| `videoTranscode.resources` | 2 CPU, 2Gi requested; 4Gi limit | Outputs are muxed in memory. |
| `videoTranscode.inputs.sizeLimit` | `64Mi` | The memory-backed `emptyDir` that holds a subtitle streamed for burn-in. |
| `videoTranscode.leaseName` | `cluster-plex-video-transcodes` | Rendered into `--video-lease-name`. |

A pod is ready only once its device has passed the start-up measure (an
h264_qsv encode through vpp_qsv, plus a VA-API decode of each format), and
stays ready while full: a seek restarts its session in the session's own
slot, and a new session a full pod refuses is passed to the next pod or
refused `full`. A pod whose device fails the measure exits, so the kubelet
restarts it with backoff. A conversion that outlives its stream by 5 s (a
GPU call that never returns) is abandoned and its slot freed; four still
running fail `/healthz`, and the kubelet restarts the pod. With kustomize, add the component
`k8s/components/video-transcode`.

## Applying a change

The manager reads its configuration once, at startup. Editing the ConfigMap
updates the mounted file but does not affect a running manager. Roll the
StatefulSet to pick up a change:

```bash
kubectl rollout restart statefulset/plex -n media
```
