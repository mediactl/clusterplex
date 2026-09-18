# Configuring the manager

The manager reads its settings from three places. Later sources win:

1. `/etc/clusterplex/config.yaml`, or the file named by `--config`
2. environment variables
3. command-line flags

Run `manager --help` for the full list of flags. Every flag has an environment
variable: replace the dashes with underscores and prefix it with
`CLUSTERPLEX_`, so `--proxy-port` reads `CLUSTERPLEX_PROXY_PORT`. The two
exceptions are `--pod-name` and `--pod-namespace`, which read the unprefixed
`POD_NAME` and `POD_NAMESPACE` that the downward API sets.

A small example:

```yaml
proxy-port: 32499
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

## Applying a change

The manager reads its configuration once, at startup. Editing the ConfigMap
updates the mounted file but does not affect a running manager. Roll the
StatefulSet to pick up a change:

```bash
kubectl rollout restart statefulset/plex -n media
```
