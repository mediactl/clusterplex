# Structured logging in cluster-plex, and Plex's log files

Date: 2026-11-10. The contract both products implement is
`clustarr/docs/superpowers/specs/2026-11-10-structured-logging-design.md`
(the record's keys, the level model, the bridging rule, the guards). This
document is cluster-plex's adoption of it and the part of the owner's brief
that is cluster-plex's alone: "manage Plex's log files via trimming,
redirection to our logs (in the correct format with labelling), and automated
deletion of the log files."

## 1. Intent

Every line a cluster-plex binary writes, and every line Plex Media Server,
its helpers, its plugins and the PostgreSQL shim write, is one record of the
shared shape on the pod's stdout, where vlagent collects it into VictoriaLogs
with the pod's labels. Plex's lines keep Plex's level, timestamp, thread and
request id. The files Plex writes are a buffer the manager drains and
empties, not a store: nothing accumulates on the node, and nothing is lost
when the pod goes.

## 2. Today

- Four binaries build `slog.NewJSONHandler(os.Stdout, nil)` by hand
  (manager, proxy, video-worker, remux-worker; the guide-build Job a fifth
  logger with `component=livetv-guide-build`). No level, no format option.
- The manager calls `slog.SetDefault`; 17 sites fall back to
  `slog.Default()` through `log()` helpers; components carry `Logger
  *slog.Logger` fields (14 packages) tagged `With("component", …)` at 36
  sites, so a third of the manager's lines carry a `component` and the rest
  none. The field name `component` collides with the pod label
  `app.kubernetes.io/component` vlagent attaches, which means something else
  (the chart's workload).
- client-go's reflector errors leave as klog text (`E1110 …
  reflector.go:227] Failed to watch …`), which vlagent parses into its own
  fields (`logger`, `source_line`, `thread_id`).
- Plex's stdout and stderr are relayed line by line at INFO with
  `source=pms-stdout|pms-stderr` (`cmd/manager/supervisor.go`'s
  `lineLogger`); the shim writes to stderr (`PLEX_PG_LOG_FILE=stderr`,
  `PLEX_PG_LOG_LEVEL=ERROR`) as `<timestamp> [ERROR] msg`, plus `[SHIM_INIT]`
  lines from the C half, all arriving as INFO.
- Plex writes its files under the state directory's `Logs/`, a per-pod
  emptyDir (`plex-logs`, no `sizeLimit`), which nobody reads. On talos
  `logDebug` is 0 (the owner's preference), so the main log is small; with
  debug on, Plex writes about 10 MB in minutes under load.
- No version is stamped into the binaries (`-ldflags "-s -w"`).

## 3. Adoption of the contract

**`pkg/logging`**, the same API as clustarr's `pkg/obs/logging` by name:
`Options{Level, Format, AddSource, Subsystems, ConfigPath}`, `New(Options)
*slog.Logger`, `FromContext`, `NewContext`, `With`, `Subsystem(ctx, name)
context.Context` and `SubsystemLogger(root, name) *slog.Logger` for
constructor injection, the config file watcher, and `Subsystems`, the table
of every subsystem name a test holds the call sites to.

**Every binary builds its logger there and nowhere else**, from the
`logging:` section of `/etc/clusterplex/config.yaml` (the ConfigMap the
manager already mounts; the proxy and workers mount the same file), the
`--log-*` flags and `LOG_LEVEL`/`LOG_FORMAT`, with the precedence the
contract gives. `service.namespace` is `cluster-plex` and `service.name` the
process (`manager`, `proxy`, `video-worker`, `remux-worker`, `maintenance`,
`guide-build`), both given to the tracer's resource as well, so the one
tracing name today, `clusterplex-manager`, becomes `manager` under the
namespace (nothing downstream keys on the old name, checked 2026-11-10);
`service.version` comes from a new `pkg/version` set by ldflags in the
Makefile and Dockerfile; `service.instance.id` is the pod name.

**Subsystems replace `component`.** The names, lowercase and dotted:
`supervisor`, `proxy`, `proxy.router`, `net.egress`, `provision`, `prefs`,
`bootstrap`, `db`, `shim`, `scanner`, `projection`, `clustarr.watch`,
`collections`, `livetv`, `livetv.converge`, `livetv.guide`,
`livetv.guide.build`, `livetv.restart`, `codecs`, `activity`, `video`,
`video.relay`, `video.worker`, `video.lease`, `remux`, `remux.relay`,
`remux.worker`, `remoteexec`, `maintenance`, `mediaproxy`, `route`,
`session`, `telemetry`, `client-go`, `pms`, `pms.scanner`, `pms.tuner`,
`pms.dlna`, `pms.transcoder`, `pms.plugin`, `pms.stdout`, `pms.stderr`. The
builder may split or rename within a package; the test holds the table and
the call sites together.

**Injection.** A component keeps its `Logger *slog.Logger` field where it
has one, built by its constructor's caller with `SubsystemLogger`; the
constructor refuses nil, so no `log()` helper and no `slog.Default()`
survive. Where a method already takes a context that carries a logger (the
collections controller, the projector's reconcile, the guide build), it reads
`FromContext`. `slog.SetDefault(root)` stays in each `main` as the drain for
libraries, never as a path our code uses. klog is bridged with
`klog.SetLogger(logr.FromSlogHandler(…))` under `client-go`.

**`err`** replaces `error` as the attribute key everywhere.

## 4. Plex's log files

### 4.1 What Plex writes

Under `<plex-dir>/Logs/`:

| File | Format | Rotation |
| --- | --- | --- |
| `Plex Media Server.log` | PMS: `Nov 10, 2026 18:20:17.104 [281472998296928] INFO - [Req#b5] message` (the thread id in brackets, the level, a dash, an optional `[Req#…]`, then the message; the server's local time, which is UTC on the pod) | at start and at 10 MB, into `.1.log` … `.N.log`, N = `LogNumFiles` (default 5) |
| `Plex Media Scanner.log`, `… Deep Analysis.log`, `… Analysis.log` | PMS | same |
| `Plex Tuner Service.log` | PMS (hex thread id) | same |
| `Plex DLNA Server.log`, `Plex Script Host.log`, `Plex Crash Uploader.log`, `Plex Image Writer.log`, `Plex Transcoder*.log` | PMS | same |
| `PMS Plugin Logs/<bundle>.log` | plugin: `2026-11-10 18:20:21,343 (ffff893ef890) :  INFO (core:349) - message` (the module and line in parentheses; levels include DEBUG, INFO, WARNING, ERROR, CRITICAL) | `.log.1` … `.log.5`, not governed by `LogNumFiles` |
| `Crash Reports/` | minidumps | not logs; out of scope (crash reporting is off) |

A line that does not start with a timestamp is a continuation of the
previous record (Plex writes SQL and stack traces that way).

### 4.2 Redirection: the tailer

A `plexlogs.Tailer` in the manager (package `pkg/plex/logs`, imported as
`plexlogs`), started with the supervisor and stopped after Plex:

- Scans `Logs/` and `Logs/PMS Plugin Logs/` every second for files matching
  the table; opens each at its start (a file that existed before the manager
  did is drained from the beginning, since nothing has read it yet), reads
  to EOF and follows, tracking each file by inode, so Plex's rename at
  rotation keeps the tailer on the old inode until it has read every line,
  and the new file at the old name is opened as new.
- Parses each line by its file's format into one record: `subsystem` from
  the file (`pms` for the main log, `pms.scanner`, `pms.tuner`, `pms.dlna`,
  `pms.transcoder`, `pms.plugin` with `plugin=<bundle id>`), `level` mapped
  (Plex `VERBOSE` TRACE, `DEBUG` DEBUG, `INFO` INFO, `WARN`/`WARNING` WARN,
  `ERROR`/`CRITICAL` ERROR), `time` Plex's own timestamp, `thread` the
  bracketed id, `req` the `Req#` value when present (removed from the
  message), `file` the file's base name, `msg` the rest of the line;
  continuation lines are appended to the previous record's `msg` with a
  newline, bounded at 64 KiB as the stdout relay is today. A line no format
  matches is one INFO record with the whole line as `msg`.
- Writes through the manager's own logger, so the shared handler applies
  the level: with the default `pms: info` Plex's DEBUG lines are dropped
  before stdout and a `subsystems: pms: debug` override turns them on live,
  independent of Plex's `logDebug`, which only decides what reaches the
  file.
- Counts `clusterplex_plex_log_lines_total{subsystem,level}` and
  `clusterplex_plex_log_bytes_total{file}`, and logs one WARN (subsystem
  `supervisor`) when it falls more than 10 MB behind a file.

The stdout and stderr relay (`lineLogger`) stays for what Plex prints
there, under `pms.stdout` and `pms.stderr`, and learns the shim's format:
`<timestamp> [DEBUG|INFO|ERROR] msg` becomes `subsystem=shim` at that level
with the shim's timestamp; `[SHIM_INIT] …` is `shim` at INFO; anything else
keeps INFO.

### 4.3 Trimming

- `LogNumFiles` is forced to `1` through `prefs` (the "forced, not
  defaulted" set), so Plex keeps one rotated copy of each file beside the
  live one: the tailer needs the rotated file only until it has finished
  reading it, and a live file is never more than 10 MB.
- The `plex-logs` emptyDir gets `sizeLimit: 1Gi` in the chart as the hard
  stop (the worst steady state is about 20 MB a file name).
- The tailer never truncates a file Plex holds open.

### 4.4 Automated deletion

- A rotated file (`*.N.log`, `*.log.N`) is deleted the moment the tailer has
  read it to EOF after its rotation, and so is any rotated file found at the
  tailer's start once drained.
- A live file nothing has written to for 24 hours (a scanner log from a run
  long finished) is drained and deleted; Plex reopens by name when it next
  writes.
- At each Plex start within the pod (Live TV's in-place restarts), the
  tailer drains what the previous process left, then deletes the rotated
  files; the live files are kept, since Plex rotates them itself at start.
- Deletions are counted (`clusterplex_plex_log_files_deleted_total{file}`)
  and logged at DEBUG.

What is given up: Plex Web's "Download logs" zip carries only the live files
and one rotated copy each. The history is in VictoriaLogs, with the
filters the files never had.

## 5. Installers and configuration

- Chart: `manager.logging` values (`level`, `format`, `addSource`,
  `subsystems`) rendered into the `logging:` section of `config.yaml`; the
  proxy and worker templates mount the same ConfigMap and bind
  `--log-config` to it; `plex-logs` with `sizeLimit: 1Gi`. When
  `manager.logging` is unset the chart reads `global.logging`, which the
  clustarr chart passes down from its own `logging` block, so talos has
  one knob for both products and a standalone install keeps its own.
- `PLEX_PG_LOG_LEVEL` stays `ERROR` by default; the shim's lines are then
  errors alone, each at ERROR.
- Nothing in cluster-bootstrap changes; a level is set in
  `clustarr/talos-values.yaml` only when the owner wants one.

## 6. Non-goals

- Plex's own log preferences beyond `LogNumFiles`: `logDebug` and
  `LogVerbose` stay the owner's, since they decide Plex's cost, not ours.
- Reading the Crash Reports directory.
- Keeping Plex's files anywhere (NFS, an object store); the store is
  VictoriaLogs.
- Parsing the message bodies into further fields (a request's path, a
  SQL statement); `msg` keeps them for full-text search.

## 7. Guards

The contract's guards, plus:

- the tailer on recorded lines from talos (the main log's start-up, an
  ERROR with `Req#`, a continuation, a tuner line, a plugin DEBUG line, a
  shim stderr line): one record each with the fields of §4.2;
- rotation: a file renamed under the tailer is read to its end and then
  deleted, and the new file at the name is read from its start, no line
  lost or doubled;
- the start sweep: rotated files present before the tailer are drained and
  removed, live ones kept;
- `LogNumFiles` is in the forced preference set;
- every subsystem name is in `Subsystems` and every constructor refuses a
  nil logger.

## 8. Left to the builder

The tailer's read buffer and the follow interval; whether the plugin
directory's own rotation needs a different inode rule (it renames the same
way); the exact list of PMS file names (from talos's `Logs/` as a start and
Plex's support article), kept as a table in the package; how the proxy and
worker pods receive the config file in the chart; the order of the
component sweep (by package, a commit each, `error` to `err` with it).
