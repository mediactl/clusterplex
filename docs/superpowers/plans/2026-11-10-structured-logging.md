# Structured logging and Plex's log files (cluster-plex) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every cluster-plex binary writes clustarr's record through one injected logger, and Plex's own log files are read into it, trimmed and deleted, so Plex's lines live in VictoriaLogs beside the manager's and nothing accumulates on the node.

**Architecture:** `pkg/logging` is clustarr's `pkg/obs/logging` by API (ported after clustarr's Tasks 1 and 2 land). Each binary builds its root from the `logging:` section of `/etc/clusterplex/config.yaml`, flags and environment; components take a required logger built with `logging.SubsystemLogger`. A `plexlogs.Tailer` in the manager follows `Logs/` and `PMS Plugin Logs/` by inode, parses each line into a record carrying Plex's own time, level, thread and request id, and deletes what it has read once Plex has rotated it; `LogNumFiles` is forced to 1 and the emptyDir capped.

**Tech Stack:** Go 1.27, `log/slog`, `k8s.io/klog/v2`, `github.com/spf13/viper` and `pflag` (as today), `gopkg.in/yaml.v3`, Prometheus client, Helm.

**Spec:** `docs/superpowers/specs/2026-11-10-structured-logging-design.md` (this repo's adoption and Plex's files), and the contract it refers to, `clustarr/docs/superpowers/specs/2026-11-10-structured-logging-design.md`. clustarr's plan, `clustarr/docs/superpowers/plans/2026-11-10-structured-logging.md`, owns the package this one ports.

## Global Constraints

- Module `github.com/mediactl/clusterplex`, Go 1.27, files without a licence header (this repo's convention).
- The record, the level names (`TRACE`…`FATAL`, `LevelTrace = -8`, `LevelFatal = 12`), the key order, `err` as the one error key, the override-by-dotted-segment rule and the precedence (file, then flags, then `LOG_LEVEL`/`LOG_FORMAT`) are clustarr's, unchanged.
- `service.namespace` is `cluster-plex`. `service.name`: `manager`, `proxy`, `video-worker`, `remux-worker`, `maintenance`, `guide-build`. The tracer's resource name `clusterplex-manager` becomes `manager` under that namespace.
- `service.version` is `pkg/version.Version`, set by `-ldflags -X github.com/mediactl/clusterplex/pkg/version.Version=<git describe>` in the Makefile and all three Dockerfiles. `service.instance.id` is `$POD_NAME`, else the hostname.
- The config is the `logging:` section of `/etc/clusterplex/config.yaml` (`--log-config`), polled every 10 s; a missing file is not an error.
- Plex's files: `<plex-dir>/Logs` and `<plex-dir>/Logs/PMS Plugin Logs`; `--plex-dir` defaults to `/var/lib/plexmediaserver/Library/Application Support/Plex Media Server`.
- Level map from Plex: `VERBOSE`→TRACE, `DEBUG`→DEBUG, `INFO`→INFO, `WARN` and `WARNING`→WARN, `ERROR` and `CRITICAL`→ERROR. Continuations are folded into one record, capped at 64 KiB.
- Deletion: a rotated file once read to its end; a live file untouched for 24 h once drained; at each PMS start, the previous process's rotated files once drained. `LogNumFiles` forced to `1`; `plex-logs` emptyDir `sizeLimit: 1Gi`.
- Metrics: `clusterplex_plex_log_lines_total{subsystem,level}`, `clusterplex_plex_log_bytes_total{file}`, `clusterplex_plex_log_files_deleted_total{file}`. `file` is the base name with its rotation index removed (`Plex Media Server.log`), so it stays bounded.
- Shared checkout: other sessions have uncommitted files here; commit with a pathspec, `git add` new files first, never `git stash`, never push.

## Review Focus

1. **Tokens in Plex's lines.** PMS logs request URLs; a line carrying `X-Plex-Token` must reach stdout with the value replaced, or every token lands in the log store. Pinned in Task 5 (`TestATokenIsRedacted`).
2. **A rotation in the middle of a line.** The last bytes of the old file without a newline are one record with whatever follows them in that file, never split across two records or joined to the new file's first line. Pinned in Task 6 (`TestARotationMidLineLosesAndDoublesNothing`).
3. **A live file recreated at start, or truncated in place.** The tailer reads the new content from its start; nothing read twice, nothing skipped. Pinned in Task 6 (`TestATruncatedLiveFileIsReadFromItsStart`).
4. **Debug logging under load.** With `logDebug` on, Plex can write faster than the tailer drains; the tailer warns once per episode of falling behind, never deletes a file it has not finished, and the emptyDir cap is the only hard stop. Pinned in Task 6 (`TestFallingBehindWarnsOnce`).
5. **A line Plex never meant as text.** Bytes that are not UTF-8, and a stack trace past 64 KiB, are one record each, cut on a rune boundary. Pinned in Task 5 (`TestAnOverlongOrBinaryLineIsOneRecord`).

---

### Task 1: `pkg/logging`, clustarr's package by API, and `pkg/version`

Start only once clustarr's plan has committed its Tasks 1 and 2.

**Files:**
- Create: `pkg/logging/{level,handler,subsystem,logging,context,config,watch,root,bridge}.go` and their tests, `pkg/version/version.go`, `pkg/version/version_test.go`
- Modify: `Makefile:44-49`, `Dockerfile:17-19` (and its other build lines), `Dockerfile.remux:25`, `Dockerfile.video` (its `go build`)

**Interfaces:**
- Produces, by the names clustarr's plan gives them: `LevelTrace`, `LevelFatal`, `ParseLevel`, `LevelName`, `Service`, `Levels` (`NewLevels`, `Set`, `For`, `OnChange`), `Handler` (`NewHandler`, `WithSubsystem`), `Options`, `New`, `FromContext`, `NewContext`, `With`, `Subsystem`, `SubsystemLogger`, `LevelsOf`, `Watch`, `FileConfig`, `ReadConfig`, `Resolve`, `BindFlags`, `DefaultPollInterval`, `WatchFile`, `Root`, `StdLogger`, `ServiceFor`, `LogrBridge`. Differences: `ServiceFor` sets `Namespace "cluster-plex"`; `BindFlags` defaults `--log-config` to `/etc/clusterplex/config.yaml`; `ReadConfig` reads that file's top-level `logging:` key with `gopkg.in/yaml.v3`.
- Produces: `version.Version string` (default `0.0.0-dev`).

- [ ] **Step 1: Port the tests first** from clustarr's `pkg/obs/logging` (the golden with `service.namespace` `cluster-plex`, the level, override, subsystem, watch and resolve tests), plus `TestVersionDefaultsToDev`.
- [ ] **Step 2: Run them to watch them fail.** Run: `go test ./pkg/logging/ ./pkg/version/ -count=1`. Expected: FAIL, the package missing.
- [ ] **Step 3: Port the implementation** and add the `-X` flag to every build line.
- [ ] **Step 4: Run them to watch them pass.** Run: `go test ./pkg/logging/ ./pkg/version/ -count=1 -race` and `make build`. Expected: PASS; `bin/manager --version` or a `strings bin/manager | grep` shows the describe string.
- [ ] **Step 5: Commit** `pkg/logging pkg/version Makefile Dockerfile Dockerfile.remux Dockerfile.video` with a pathspec.

---

### Task 2: The manager's root, its tracer name and klog

**Files:**
- Modify: `cmd/manager/main.go:157-176` (root, telemetry), `cmd/manager/config.go` (the `logging` section and `--log-*` flags on its pflag set), `cmd/manager/cloudguidejob.go:429` (the guide-build Job), `pkg/telemetry/otel.go` (resource `service.namespace`, `service.version`; its `component: otel` logger becomes the `telemetry` subsystem)
- Test: `cmd/manager/logging_test.go`, `pkg/telemetry/otel_test.go`

**Interfaces:**
- Consumes: Task 1.
- Produces: `run()` builds the root with `logging.Root(ctx, opts, logging.ServiceFor("manager"))` after `loadConfig`; a `loadConfig` error is written through `logging.New(logging.Options{})`. `klog.SetLogger(logging.LogrBridge(SubsystemLogger(root, "client-go")))`. The guide-build subcommand's root is `ServiceFor("guide-build")` with subsystem `livetv.guide.build`. `telemetry.OTel` sets `service.namespace=cluster-plex`, `service.version`.

- [ ] **Step 1: Write the failing tests.** `TestTheManagerLogsTheContract`: `run`'s logger setup factored into `newRoot(cfg Config, out io.Writer)`; its first line carries `service.name=manager`, `service.namespace=cluster-plex`. `TestKlogReachesClientGo`: `klog.ErrorS(err,"Failed to watch")` is one ERROR record, `subsystem: client-go`, with `err`. `TestTheTracerIsManagerUnderClusterPlex`: the resource has `service.name=manager`, `service.namespace=cluster-plex`. `TestTheGuideBuildJobNamesItsService`: `service.name=guide-build`.
- [ ] **Step 2: Run them to watch them fail.** Run: `go test ./cmd/manager/ ./pkg/telemetry/ -run 'TestTheManager|TestKlog|TestTheTracer|TestTheGuideBuild' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement**; `k8s.io/klog/v2` becomes a direct dependency, added serially.
- [ ] **Step 4: Run them to watch them pass.** Run: `go test ./cmd/manager/ ./pkg/telemetry/ -count=1`. Expected: PASS.
- [ ] **Step 5: Commit** with a pathspec.

---

### Task 3: Components become subsystems, injected and required

**Files:**
- Create: `pkg/logging/subsystems.go` (the table), `test/guards/logging_test.go` (new package `guards` under `test/`, its own AST helpers)
- Modify: the 26 structs with a `Logger *slog.Logger` field, the 20 `log()`/`logger()` helpers, the 15 files calling `slog.Default()`, the 37 `"component"` sites, the 200 `"error"` keys (sites as the guards list them)

**Interfaces:**
- Consumes: `logging.SubsystemLogger`, `logging.Subsystem`.
- Produces: `var Subsystems []string`, holding the spec's list (`supervisor`, `proxy`, `proxy.router`, `net.egress`, `provision`, `prefs`, `bootstrap`, `db`, `shim`, `scanner`, `projection`, `clustarr.watch`, `collections`, `livetv`, `livetv.converge`, `livetv.guide`, `livetv.guide.build`, `livetv.restart`, `codecs`, `activity`, `video`, `video.relay`, `video.worker`, `video.lease`, `remux`, `remux.relay`, `remux.worker`, `remoteexec`, `maintenance`, `mediaproxy`, `route`, `session`, `telemetry`, `client-go`, `stdlog`, `logging`, `pms`, `pms.scanner`, `pms.tuner`, `pms.dlna`, `pms.transcoder`, `pms.plugin`, `pms.stdout`, `pms.stderr`) and the splits this task makes: `livetv-cloud-guide`→`livetv.guide.cloud`, `livetv-xmltv-guide`→`livetv.guide.xmltv`, `livetv-guides`→`livetv.guide`, `livetv-grabs`→`livetv.grabs`, `server-token`→`token`, `plex-postgresql-init`→`db`, `plexnet`→`net`, `egress`→`net.egress`, `router`→`proxy.router`, `executor`→`remoteexec.executor`, `dispatcher`→`remoteexec.dispatcher`, `clustarr`→`clustarr.watch`, `otel`→`telemetry`, `video-lease`→`video.lease`; every other component keeps its name.
- Each type with a `Logger` field refuses a nil one where its work starts: a `New…` returns an error, a `Run`/`Start`/`Serve` returns `errNoLogger` before doing anything.

- [ ] **Step 1: Write the failing guards.** `TestNoGlobalLogger` (no `slog.Default()`, `slog.New(` or `slog.SetDefault` outside `pkg/logging`, `slog.New(slog.DiscardHandler)` in tests only); `TestNoLoggerInventingHelper` (no method named `log` or `logger` returning `*slog.Logger`); `TestNoComponentKey` (no `"component"` attribute key); `TestNoErrorAttributeKey` (clustarr's rule); `TestEverySubsystemIsInTheTable` (literal names, all in the table, every entry used); `TestNoStdlibLogOrStderrPrints` with `cmd/shim` (it runs as Plex's transcoder stand-in, whose stderr Plex reads) and `cmd/maintenance`'s usage error named as exemptions. Plus, per type, a test that a nil logger is refused (table-driven in each package that has such a type).
- [ ] **Step 2: Run them to watch them fail.** Run: `go test ./test/guards/ -count=1`. Expected: FAIL, listing the sites.
- [ ] **Step 3: Sweep by package**, one commit each: the caller builds each logger with `SubsystemLogger(root, name)`; the helper goes; `"error"` becomes `"err"`.
- [ ] **Step 4: Run them to watch them pass.** Run: `go test ./... -count=1` (the repo's unit suites) and `go build ./...`. Expected: PASS.
- [ ] **Step 5: Commit** each package with a pathspec.

---

### Task 4: The proxy, the workers and the maintenance Job read the same file

**Files:**
- Modify: `cmd/proxy/main.go:41-42`, `cmd/video-worker/main.go:125-138`, `cmd/remux-worker/main.go:52-57`, `cmd/maintenance/main.go`, `charts/cluster-plex/templates/{video-transcode,video-transcode-cpu,remux,maintenance}.yaml` (mount the `-config` ConfigMap at `/etc/clusterplex`, read-only, no `subPath`)
- Test: `cmd/*/main_test.go` (one each), `test/manifests/logging_test.go`

**Interfaces:**
- Produces: each binary binds `logging.BindFlags` on its flag set and calls `logging.Root` with its `ServiceFor(...)` name. `maintenance` keeps its CLI usage error before the logger exists.

- [ ] **Step 1: Write the failing tests.** Per binary, `TestItsFirstLineCarriesItsService` (the binary's setup factored to take an `io.Writer`). `TestEveryPodMountsTheConfig`: `helm template` of the chart with default values; every Deployment, StatefulSet, Job and CronJob running a cluster-plex binary mounts the config ConfigMap at `/etc/clusterplex` without `subPath`.
- [ ] **Step 2: Run them to watch them fail.** Run: `go test ./cmd/... ./test/manifests/ -run 'TestItsFirstLine|TestEveryPodMountsTheConfig' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them to watch them pass.** Expected: PASS; `helm lint --strict charts/cluster-plex`.
- [ ] **Step 5: Commit** with a pathspec.

---

### Task 5: Reading Plex's lines

**Files:**
- Create: `pkg/plex/logs/parse.go` (package `plexlogs`), `pkg/plex/logs/files.go` (the table of file names), `pkg/plex/logs/testdata/*.log`
- Test: `pkg/plex/logs/parse_test.go`

**Interfaces:**
- Produces:
  - `type Record struct{ Time time.Time; Level slog.Level; Subsystem, Thread, Req, File, Plugin, Msg string }`
  - `type Format int` with `FormatPMS`, `FormatPlugin`
  - `func FileFor(base string) (subsystem string, format Format, ok bool)`: `Plex Media Server.log`→`pms`; `Plex Media Scanner*.log`→`pms.scanner`; `Plex Tuner Service.log`→`pms.tuner`; `Plex DLNA Server.log`→`pms.dlna`; `Plex Transcoder*.log`→`pms.transcoder`; the other `Plex *.log` files→`pms`; a file in `PMS Plugin Logs/`→`pms.plugin` with `Plugin` its bundle id; rotated names map as their live name.
  - `func Stem(base string) string`: the base name without its rotation index (`X.3.log`→`X.log`, `X.log.2`→`X.log`).
  - `type Assembler struct{...}`: `NewAssembler(file string, subsystem string, format Format) *Assembler`, `(*Assembler).Line(b []byte) (Record, bool)` returns the record a new line completes, `(*Assembler).Flush() (Record, bool)` the one still open.

PMS layout: `Jan 02, 2006 15:04:05.000 [thread] LEVEL - [Req#id] message`, the `[Req#…]` optional and removed from `Msg`, thread decimal or hex, time read as UTC. Plugin layout: `2006-01-02 15:04:05,000 (thread) :  LEVEL (module:line) - message`. A line matching neither continues the open record; a first line matching neither is one INFO record. `Msg` replaces `X-Plex-Token=<value>` and `X-Plex-Token: <value>` with `X-Plex-Token=REDACTED`, is cut to 64 KiB on a rune boundary, and invalid UTF-8 is replaced with U+FFFD.

- [ ] **Step 1: Record the fixtures.** Fetch a log bundle from talos's PMS read-only (`GET /diagnostics/logs`, the token from Secret `clustarr-system/plex-token` into an environment variable, never printed); keep a start-up block of `Plex Media Server.log`, an ERROR line with a `Req#`, a multi-line SQL continuation, a `Plex Tuner Service.log` line, and a plugin DEBUG line. Replace every token, address and account name with placeholders before the files are added. Without the bundle, write them from the spec's §4.1 samples.
- [ ] **Step 2: Write the failing tests.** `TestPMSLines` (time, level, thread, req, msg for each fixture line); `TestPluginLines` (module and line stay in `Msg`, `WARNING`→WARN, `CRITICAL`→ERROR); `TestContinuationsFold` (the SQL block is one record whose `Msg` holds every line joined by `\n`); `TestFileFor` and `TestStem` tables; `TestATokenIsRedacted` (`…?X-Plex-Token=abc123&x=1` reads `X-Plex-Token=REDACTED&x=1`, case-insensitive); `TestAnOverlongOrBinaryLineIsOneRecord` (a 100 KiB continuation is one record of 64 KiB ending on a rune boundary; a line with bytes `0xff 0xfe` is one record with U+FFFD).
- [ ] **Step 3: Run them to watch them fail.** Run: `go test ./pkg/plex/logs/ -count=1`. Expected: FAIL.
- [ ] **Step 4: Implement, run to pass.** Expected: PASS.
- [ ] **Step 5: Commit** with a pathspec, the fixtures included.

---

### Task 6: The tailer: follow, write, delete

**Files:**
- Create: `pkg/plex/logs/tailer.go`, `pkg/plex/logs/metrics.go`
- Test: `pkg/plex/logs/tailer_test.go`

**Interfaces:**
- Consumes: Task 5.
- Produces:
  - `type Tailer struct{ Dir string; Logger *slog.Logger; Interval, StaleAfter time.Duration; BehindWarn int64; Metrics *Metrics; Now func() time.Time }` with defaults 1 s, 24 h, 10 MiB.
  - `func (t *Tailer) Run(ctx context.Context) error`: refuses a nil `Logger`; scans `Dir` and `Dir/PMS Plugin Logs` each `Interval`; drains every file it finds at start from its beginning; follows each by inode (device and inode from `syscall.Stat_t`); returns when `ctx` ends after one last drain.
  - `func (t *Tailer) Drain(ctx context.Context) error`: reads every followed file to its end, then deletes every rotated file read to its end (the supervisor calls it at each PMS start).
  - `func NewMetrics(reg prometheus.Registerer) *Metrics`
- Each record is written with Plex's time: `slog.NewRecord(rec.Time, rec.Level, rec.Msg, 0)` with `thread`, `req`, `file`, `plugin` attributes added when set, through `SubsystemLogger(t.Logger, rec.Subsystem).Handler()`, after its `Enabled` check.

Rules: a file whose live name now belongs to another inode is rotated; it is read to its end and then deleted by whatever name its inode carries now (Plex may have renamed it again), or forgotten if Plex already removed it. A live file not written for `StaleAfter` is drained and deleted. A live file whose size dropped below the offset read is read again from 0. A file's partial last line waits for its newline, and is flushed as a record when the file is rotated or deleted. When the unread bytes across files pass `BehindWarn`, one WARN with `subsystem: supervisor` and the byte count is written, and not again until the backlog has once fallen below it.

- [ ] **Step 1: Write the failing tests**, each over a temporary directory written as Plex writes, with an injected clock: `TestALineIsOneRecordWithPlexsTime`; `TestARotatedFileIsReadToItsEndThenDeleted` (rename `X.log`→`X.1.log`, create a new `X.log`: every line of both read once, `X.1.log` deleted, `clusterplex_plex_log_files_deleted_total{file="X.log"}` 1); `TestARotationMidLineLosesAndDoublesNothing` (the old file ends without a newline, the new one starts with a full line: two records, the old one's text whole); `TestATruncatedLiveFileIsReadFromItsStart`; `TestFilesPresentAtStartAreDrainedAndRotatedOnesRemoved` (a pre-existing `X.1.log` and `X.log`: both read, only `X.1.log` deleted); `TestAStaleLiveFileIsDeleted` (24 h of the clock); `TestFallingBehindWarnsOnce` (`BehindWarn` 1 KiB, 4 KiB written between ticks twice: one WARN); `TestAnOverrideQuietsPlex` (`pms: warn` drops an INFO line from `Plex Media Server.log`); `TestTheMetricsCount` (`lines_total{subsystem="pms",level="ERROR"}`, `bytes_total{file="Plex Media Server.log"}`); `TestRunRefusesANilLogger`.
- [ ] **Step 2: Run them to watch them fail.** Run: `go test ./pkg/plex/logs/ -run 'TestA|TestFiles|TestFalling|TestThe|TestRun' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement, run to pass,** `-race` included.
- [ ] **Step 4: Commit** with a pathspec.

---

### Task 7: Wiring it into Plex's pod

**Files:**
- Modify: `cmd/manager/supervisor.go` (start the tailer with Plex, `Drain` before each start, stop it after Plex; `lineLogger` writes `pms.stdout` and `pms.stderr` and parses the shim's lines), `pkg/plex/prefs/required.go` (`LogNumFiles` = `1` in `Required`), `charts/cluster-plex/templates/plex.yaml:214` (`plex-logs` `emptyDir: {sizeLimit: 1Gi}`), `cmd/manager/main.go` (metrics registration)
- Test: `cmd/manager/supervisor_test.go`, `pkg/plex/prefs/required_test.go`, `test/manifests/logging_test.go`

**Interfaces:**
- The shim's line, from `plex-postgresql/rust/plex-pg-core/src/pg_logging.rs`: `[YYYY-MM-DD HH:MM:SS] [DEBUG|INFO|ERROR] message`; such a line is a record with `subsystem: shim`, its level and its time (UTC); a line starting `[SHIM_INIT]` is `shim` at INFO; any other line keeps INFO under `pms.stdout` or `pms.stderr`.

- [ ] **Step 1: Write the failing tests.** `TestShimLinesTakeTheirLevel` (`[2026-11-10 18:20:17] [ERROR] statement timeout` is ERROR `shim` with that time; `[SHIM_INIT] Exception catcher enabled` is INFO `shim`; `hello` is INFO `pms.stderr`); `TestTheSupervisorDrainsBeforeEachStart` (a fake PMS start with a rotated file present: drained and deleted before the process starts); `TestLogNumFilesIsForced` (`Required()["LogNumFiles"] == "1"`, and declaring it is refused by `Validate`); `TestPlexLogsIsCapped` (`helm template`: the `plex-logs` emptyDir has `sizeLimit: 1Gi`).
- [ ] **Step 2: Run them to watch them fail.** Run: `go test ./cmd/manager/ ./pkg/plex/prefs/ ./test/manifests/ -run 'TestShimLines|TestTheSupervisorDrains|TestLogNumFiles|TestPlexLogsIsCapped' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement, run to pass.**
- [ ] **Step 4: Commit** with a pathspec.

---

### Task 8: The chart's `logging` values

**Files:**
- Modify: `charts/cluster-plex/values.yaml` (`manager.logging`, documented), `charts/cluster-plex/templates/storage.yaml` (render `logging:` into `config.yaml`), `charts/cluster-plex/values.schema.json` if present
- Test: `test/manifests/logging_test.go`

The section rendered is `global.logging` with `manager.logging` over it key by key, its `subsystems` merged over the global's; the default renders `logging: {level: info, format: json, addSource: false}`. Under clustarr's chart, `global.logging` is the owner's one knob (clustarr's plan, Task 8).

- [ ] **Step 1: Write the failing test.** `TestTheLoggingSectionRenders`: default values render the default section; `global.logging.level: debug` renders `debug`; `manager.logging.subsystems: {pms: warn}` beside `global.logging.subsystems: {livetv: debug}` renders both; `manager.logging.level: error` beats the global.
- [ ] **Step 2: Run to fail, implement, run to pass.** Run: `go test ./test/manifests/ -run TestTheLoggingSectionRenders -count=1`; `helm lint --strict charts/cluster-plex`.
- [ ] **Step 3: Commit** with a pathspec.

---

### Task 9: Documentation and the final verification

- [ ] **Step 1:** The chart README's `logging` values and Plex's files (what is kept, what is deleted, what "Download logs" now carries); the repo's observability notes, if any, gain the three metrics.
- [ ] **Step 2:** Run `make build`, `go vet ./...`, `go test ./... -count=1`, `helm lint --strict charts/cluster-plex`. Expected: green; any red test named with its output.
- [ ] **Step 3: Commit** with a pathspec.

---

## Notes for the executor

- **Order across the two repos:** clustarr's Tasks 1 and 2 first; this plan's Task 1 ports them; the rest of each plan is independent of the other.
- **Rulings already made in this plan:** the knob is `global.logging` with `manager.logging` over it (Helm hands a subchart only the parent's `global`); the components are renamed by the table in Task 3; a type with a `Logger` field refuses nil where its work starts, since most are struct literals with no constructor; `cmd/shim` is exempt from the stderr guard as Plex's own child.
- **Deploying it** is not part of this plan. Forcing `LogNumFiles` takes effect at Plex's next start; check `clusterplex_plex_sessions` before any restart of plex-0.
