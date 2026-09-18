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
plex:
  preferences:
    - name: customConnections
      value: https://plex.example.com:443
```

Use the address clients actually reach — the `plex-main` LoadBalancer, or
whatever ingress sits in front of it. Plex treats this as an additional
connection rather than a replacement, so it is additive and safe to set.

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
