# Cluster Plex

Plex Media Server on Kubernetes, as a StatefulSet where one pod runs the server
and the rest do work for it. LiteFS replicates Plex's SQLite databases so any
pod can take over; a shim replaces Plex's helper binaries so transcodes run on
other pods.

Go module `github.com/mediactl/clusterplex`. One image, two binaries.

## Read before designing anything

- `docs/media-proxy-pattern.md` — the direction of travel: a dedicated proxy
  service that serves media bytes itself so aggregate bandwidth scales past one
  node's interface, coordinated through the Lease rather than pod labels.
- `docs/adr/0001-litefs-over-mvsqlite.md` — why LiteFS and not a multi-writer
  SQLite. Plex cannot run as several coordinated instances; that constraint
  shapes everything else.
- `docs/adr/0002-proxy-plex-through-port-redirect.md` — why a TCP proxy plus a
  nat REDIRECT rather than an HTTP reverse proxy.
- `docs/configuration.md` — the config system and how Plex preferences are
  managed.

## Layout

| Path | Owns |
| --- | --- |
| `cmd/manager/` | Runs in every pod: LiteFS, leader election, the Plex supervisor, the job listeners |
| `cmd/shim/` | Stands in for Plex's helper binaries and forwards each invocation to the manager |
| `pkg/litefsk8s/` | LiteFS leader election on a Kubernetes Lease |
| `pkg/remoteexec/` | Running helper binaries locally or on a worker pod |
| `pkg/plexprefs/` | Merging declared settings into Plex's `Preferences.xml` |
| `pkg/proxy/`, `pkg/portredirect/` | The L4 proxy in front of Plex and the rule that steers traffic into it |
| `third_party/litefs/` | Upstream LiteFS plus our patches. **Materialised, never committed** |

## Invariants

- **One pod runs Plex.** The Lease decides which. Losing it exits the process so
  the pod restarts clean.
- **The Lease is the only source of truth for leadership.** Do not reintroduce a
  derived copy such as a pod label; a cache can disagree, and one did.
- **Plex's `Preferences.xml` is merged, never regenerated.** It holds the server
  identity and the plex.tv token, which cannot be reconstructed.
- **LiteFS lineage is never resolved automatically.** See the cluster ID gotcha
  below; guessing wrong destroys data.
- **Plex's state directory is shared, LiteFS's is per pod.** The first carries
  identity and metadata; the second is a replica and must not be shared.

## Commands

```bash
make litefs        # materialise third_party/litefs (upstream tag + hack/litefs patches)
make build         # manager and shim into bin/
make test          # unit tests
make lint          # golangci-lint v2
make docker-build  # the image; it runs the litefs fetch itself
make kind-up kind-load deploy-kind
make e2e           # deploys the kind overlay and runs the end-to-end test
```

The end-to-end test is behind the `e2e` build tag so `go test ./...` stays
hermetic.

## Gotchas found the hard way

- **Plex binds 32400 *and* 32401.** It exits with "Error binding acceptor:
  Address in use" if either is taken, within a tenth of a second and with the
  reason only in its own log. The proxy therefore defaults to 32499.
- **Plex has no setting for its listen address or port.** Getting traffic to go
  through anything else needs a nat REDIRECT, which is why the pod needs
  `iptables`.
- **`ProcessedMachineIdentifier` is what clients see as the server ID.** Plex
  derives it from `MachineIdentifier` with a salt you cannot reproduce, and
  **never recomputes it**. Change the UUID without deleting the derived value
  and clients keep seeing the old server forever.
- **The LiteFS cluster ID must live on the Lease.** It used to be generated per
  node, so every pod that became primary minted its own and LiteFS then refused
  to replicate between them, permanently and silently. Worse: a node that
  disagrees is not necessarily the stale one, because whichever pod wins the
  election first stamps its lineage. Resolving that automatically already
  destroyed a replica's data once. It is an operator decision, behind
  `--litefs-adopt-cluster-id`.
- **Viper lowercases nested map keys.** `FriendlyName` silently becomes
  `friendlyname`, and Plex preference names are case sensitive. Preferences are
  a list of name/value pairs for that reason.
- **A stale `plexmediaserver.pid` stops Plex starting.** It outlives the
  container on a persistent volume and container PIDs repeat, so the supervisor
  removes it before every start.
- **`go test ./...` passing does not mean the cluster works.** Nearly every bug
  in this repo so far was only visible in a real cluster.
- **The image has no `curl`,** and Plex's bundled ffmpeg is stripped of `lavfi`,
  so generate test media on the host and copy it in. Plex writes a local admin
  token to `.LocalAdminToken` in its state directory, which is the way to call
  the API on a claimed server.

## Conventions

- Logging is `slog` with a handler per component; no package-level logger.
- Tests are table-driven with testify, and name the behaviour rather than the
  function. Anything worth a comment about *why* goes in the test name or a
  comment above it.
- Commit messages are plain imperative sentences, matching this repo's history.
