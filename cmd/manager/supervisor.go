package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	plexlogs "github.com/mediactl/clusterplex/pkg/plex/logs"

	"github.com/mediactl/clusterplex/pkg/logging"
	"github.com/mediactl/clusterplex/pkg/proxy"
)

const (
	defaultGrace = 30 * time.Second
	maxLogLine   = 64 * 1024
)

// Supervisor owns the Plex Media Server process on the leader: the port proxy
// in front of it, the redirect that steers inbound traffic through that proxy,
// and an orderly stop that lets Plex flush its databases.
type Supervisor struct {
	Binary string
	// Subreaper, when set, is run in place of Plex and given Plex to run.
	//
	// Plex re-execs itself through vfork. The process we started then exits
	// cleanly and its replacement is reparented to pid 1, so a supervisor
	// watching only what it launched sees a healthy exit and restarts the pod,
	// on a loop. The subreaper sets PR_SET_CHILD_SUBREAPER, so the replacement
	// is reparented to it instead, and it waits for the last descendant.
	Subreaper string
	// StateDir is Plex's state directory. The directories under it that Plex
	// expects to find are created before each start; see plexStateDirs.
	StateDir string
	Logger   *slog.Logger
	// PIDFile is Plex's pid file. Plex refuses to start when it names a live
	// process; on a persistent volume the file outlives the container and
	// container pids repeat, so it is removed before every start.
	PIDFile string
	// Env is the environment Plex is started with. Nil inherits this
	// process's own. It carries the preload that redirects Plex's database
	// calls to PostgreSQL, so getting it wrong means Plex silently falls back
	// to its own SQLite file.
	Env []string
	// Grace is how long Plex gets after SIGTERM before it is killed.
	Grace time.Duration
	// Drain is how long Stop lets the client connections the proxy holds
	// finish before Plex is told to stop. Zero stops Plex at once.
	//
	// Restart never drains: a Plex that has stopped answering is holding
	// nothing its clients could still get.
	Drain time.Duration
	// StartProcess, when set, starts Plex in place of cmd.Start(), so it can
	// be launched inside its own network namespace.
	StartProcess func(*exec.Cmd) error
	// Preferences, when set, writes Plex's Preferences.xml before each start.
	// Plex reads that file once at startup, so it has to run first. A failure
	// is fatal: there is no fallback for settings that silently fail to apply,
	// and starting Plex anyway would run it with a configuration that differs
	// from the declared one.
	Preferences func(ctx context.Context) error
	// BeforeStart, when set, runs before each start after Preferences: the
	// Live TV guides' schemas and shadows (prepareGuides), which the shim
	// reads when PMS opens them. A failure is logged, not fatal: a guide
	// without its schema stays the SQLite file it was.
	BeforeStart func(ctx context.Context) error
	// Proxy, when set, starts before Plex and stops after it.
	Proxy *proxy.TCP
	// Logs, when set, reads Plex's own log files into the manager's records
	// (the 2026-11-10 structured-logging design §4): drained before each
	// start, for what the previous process left, run from the first start
	// and stopped after Plex.
	Logs *plexlogs.Tailer
	// OnUnexpectedExit is called when Plex exits without Stop having been called.
	OnUnexpectedExit func(err error)

	mu         sync.Mutex
	cmd        *exec.Cmd
	stopping   bool
	done       chan struct{}
	cancel     context.CancelFunc
	logsCancel context.CancelFunc
}

// Start applies the redirect, starts the proxy and then Plex.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		return errors.New("plex media server is already running")
	}

	// The proxy and Plex live until Stop, not until the caller's context.
	// Everything in the manager runs under the context the shutdown signal
	// cancels, and every proxied connection closes when its context does; so
	// while the proxy shared that context, SIGTERM closed every client
	// connection before the drain could look, and the drain found nothing to
	// wait for. Stop cancels this one, after the drain and after Plex.
	pctx, cancel := context.WithCancel(context.Background())
	if s.Proxy != nil {
		addr, err := s.Proxy.Start(pctx)
		if err != nil {
			cancel()
			return fmt.Errorf("start proxy: %w", err)
		}
		s.Logger.Info("proxy listening", "addr", addr.String(), "target", s.Proxy.Target, "drain", s.Drain)
	}

	if s.Preferences != nil {
		if err := s.Preferences(ctx); err != nil {
			cancel()
			if s.Proxy != nil {
				s.Proxy.Wait()
			}
			return fmt.Errorf("apply Plex preferences: %w", err)
		}
	}
	if s.BeforeStart != nil {
		if err := s.BeforeStart(ctx); err != nil {
			s.Logger.Warn("prepare before Plex starts", "err", err)
		}
	}

	s.removeStalePIDFile()
	s.ensureStateDirs()
	if s.Logs != nil {
		// What the previous process left: read, and its rotated files gone,
		// before Plex rotates the live ones at its start (design §4.4).
		if err := s.Logs.Drain(ctx); err != nil {
			s.Logger.Warn("drain Plex's log files before the start", "err", err)
		}
		if s.logsCancel == nil {
			lctx, lcancel := context.WithCancel(context.Background())
			s.logsCancel = lcancel
			go func() {
				if err := s.Logs.Run(lctx); err != nil {
					s.Logger.Error("Plex's log files are not read", "err", err)
				}
			}()
		}
	}
	cmd := exec.Command(s.Binary)
	if s.Subreaper != "" {
		cmd = exec.Command(s.Subreaper, s.Binary)
	}
	cmd.Env = s.Env
	cmd.Stdout = &lineLogger{log: s.Logger, subsystem: "pms.stdout"}
	cmd.Stderr = &lineLogger{log: s.Logger, subsystem: "pms.stderr"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := s.startProcess(cmd); err != nil {
		cancel()
		if s.Proxy != nil {
			s.Proxy.Wait()
		}
		return fmt.Errorf("start plex media server: %w", err)
	}
	s.cmd, s.cancel, s.done, s.stopping = cmd, cancel, make(chan struct{}), false
	s.Logger.Info("Plex Media Server started", "pid", cmd.Process.Pid)
	go s.wait(cmd)
	return nil
}

// startProcess launches Plex, through StartProcess when one is set. There is
// deliberately no fallback if that fails: Plex binds 32400 itself, which in
// the pod namespace belongs to the proxy, so starting it in the wrong
// namespace means an immediate "Address in use" with the reason recorded only
// in Plex's own log.
func (s *Supervisor) startProcess(cmd *exec.Cmd) error {
	if s.StartProcess != nil {
		return s.StartProcess(cmd)
	}
	return cmd.Start()
}

// plexStateDirs are the directories Plex expects to find under its state
// directory. It stats them rather than creating them, and an absent one raises
// a boost::filesystem exception that nothing catches, so Plex dies within a
// second of starting. On a fresh volume none of them exist.
var plexStateDirs = []string{"Plug-ins", "Metadata", "Cache", "Logs", "Crash Reports"}

// ensureStateDirs creates those directories, leaving any that exist untouched.
// It runs on every start, against a volume that may hold a real library, so it
// only ever adds.
func (s *Supervisor) ensureStateDirs() {
	if s.StateDir == "" {
		return
	}
	for _, name := range plexStateDirs {
		path := filepath.Join(s.StateDir, name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			// Not fatal here: Plex reports the one it actually wanted, which is
			// more useful than guessing which of these mattered.
			s.Logger.Error("create Plex state directory", "path", path, "err", err)
		}
	}
}

func (s *Supervisor) removeStalePIDFile() {
	if s.PIDFile == "" {
		return
	}
	err := os.Remove(s.PIDFile)
	switch {
	case err == nil:
		s.Logger.Info("removed stale Plex pid file", "path", s.PIDFile)
	case !errors.Is(err, os.ErrNotExist):
		s.Logger.Warn("remove Plex pid file", "path", s.PIDFile, "err", err)
	}
}

func (s *Supervisor) wait(cmd *exec.Cmd) {
	err := cmd.Wait()
	s.mu.Lock()
	stopping, done := s.stopping, s.done
	s.mu.Unlock()
	close(done)

	if stopping {
		s.Logger.Info("Plex Media Server stopped", "err", err)
		return
	}
	if err == nil {
		err = errors.New("exited with status 0")
	}
	s.Logger.Error("Plex Media Server exited unexpectedly", "err", err)
	if s.OnUnexpectedExit != nil {
		s.OnUnexpectedExit(err)
	}
}

// lostGrace is how long Plex gets to exit when it is being restarted because
// it stopped answering, rather than because someone asked it to stop.
//
// It is deliberately much shorter than Grace. By the time Restart is called
// the health watch has already waited unhealthyRestartAfter checks, so Plex is
// not merely slow — and the subreaper it runs under will not exit on SIGTERM
// while any descendant lives, which is the whole reason the supervisor did not
// notice by itself. Waiting the full grace here just adds half a minute to
// every recovery before the inevitable SIGKILL.
const lostGrace = 3 * time.Second

// pid is the process the supervisor is currently waiting on, or 0.
func (s *Supervisor) pid() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// Restart replaces a Plex that is no longer serving.
//
// It exists because Plex can die without the supervisor learning of it: Plex
// runs under a subreaper, which stays alive while any descendant does, so
// cmd.Wait blocks and OnUnexpectedExit never fires. The health watch is what
// notices, and this is what it should do about it — replacing the process
// takes seconds, where replacing the pod took forty and re-ran the init
// script and the election on the way back.
//
// Stopping first is not optional even though Plex is expected to be gone
// already: the subreaper and whatever descendants kept it alive are still
// there, and they hold Plex's port.
func (s *Supervisor) Restart(ctx context.Context) error {
	if err := s.stopWithin(ctx, lostGrace); err != nil {
		return fmt.Errorf("stop the Plex that stopped answering: %w", err)
	}
	return s.Start(ctx)
}

// Stop lets the connections the proxy holds finish, for up to Drain, then
// sends SIGTERM to Plex's process group, waits up to Grace, kills it, and
// finally stops the proxy. It is a no-op when nothing is running.
func (s *Supervisor) Stop(ctx context.Context) error {
	grace := s.Grace
	if grace == 0 {
		grace = defaultGrace
	}
	s.drain(ctx)
	err := s.stopWithin(ctx, grace)
	// The tailer stops after Plex, with one last drain of what it wrote
	// on its way out.
	s.mu.Lock()
	lcancel := s.logsCancel
	s.logsCancel = nil
	s.mu.Unlock()
	if lcancel != nil {
		lcancel()
	}
	return err
}

// drain waits for the clients this pod is serving to finish with it.
//
// A terminating pod has already left its Service, so no new client arrives;
// the ones it holds are mid-stream, and a transcode's chunks live only here.
// Before this, a rollout cut every one of them at the instant it began.
func (s *Supervisor) drain(ctx context.Context) {
	if s.Proxy == nil || s.Drain <= 0 {
		s.Logger.Info("not draining client connections: draining is off", "drain", s.Drain)
		return
	}
	s.mu.Lock()
	running := s.cmd != nil
	s.mu.Unlock()
	if !running {
		s.Logger.Info("not draining client connections: Plex Media Server is not running")
		return
	}
	open, streams := s.Proxy.Open(), s.Proxy.Streams()
	if streams == 0 {
		s.Logger.Info("no streams to drain", "idle_connections", open)
		return
	}
	s.Logger.Info("draining client connections before stopping Plex Media Server",
		"streams", streams, "open", open, "timeout", s.Drain)
	if left := s.Proxy.Drain(ctx, s.Drain); left > 0 {
		s.Logger.Warn("stopping Plex Media Server with client connections still open",
			"streams", left, "waited", s.Drain)
		return
	}
	s.Logger.Info("client connections drained", "idle_connections_closed", s.Proxy.Open())
}

func (s *Supervisor) stopWithin(ctx context.Context, grace time.Duration) error {
	s.mu.Lock()
	cmd, done, cancel := s.cmd, s.done, s.cancel
	s.stopping = true
	s.cmd = nil
	s.mu.Unlock()
	if cmd == nil {
		return nil
	}
	defer func() {
		cancel()
		if s.Proxy != nil {
			s.Proxy.Wait()
		}
	}()

	s.Logger.Info("stopping Plex Media Server", "pid", cmd.Process.Pid, "grace", grace)
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)

	select {
	case <-done:
		return nil
	case <-time.After(grace):
		s.Logger.Warn("Plex Media Server ignored SIGTERM, killing")
	case <-ctx.Done():
		s.Logger.Warn("stop cancelled, killing Plex Media Server")
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	<-done
	return ctx.Err()
}

// lineLogger relays a process's output one line at a time as records under
// subsystem (pms.stdout, pms.stderr), each INFO -- except the PostgreSQL
// shim's own lines, "[time] [LEVEL] message" and "[SHIM_INIT] ...", which
// are shim's at their level and time (the 2026-11-10 design §4.2).
type lineLogger struct {
	log       *slog.Logger
	subsystem string
	mu        sync.Mutex
	buf       []byte
}

// shimLine is the shim's log line (plex-postgresql's pg_logging.rs).
var shimLine = regexp.MustCompile(`^\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\] \[(DEBUG|INFO|ERROR)\] (.*)$`)

const shimInit = "[SHIM_INIT] "

func (w *lineLogger) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > maxLogLine {
		w.emit(w.buf)
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

func (w *lineLogger) emit(line []byte) {
	text := string(bytes.TrimSpace(line))
	if text == "" {
		return
	}
	subsystem, level, at, msg := w.subsystem, slog.LevelInfo, time.Now(), text
	if m := shimLine.FindStringSubmatch(text); m != nil {
		subsystem, msg = "shim", m[3]
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], time.UTC); err == nil {
			at = t
		}
		level = plexlogs.Level(m[2])
	} else if rest, ok := strings.CutPrefix(text, shimInit); ok {
		subsystem, msg = "shim", rest
	}
	h := logging.SubsystemLogger(w.log, subsystem).Handler()
	ctx := context.Background()
	if !h.Enabled(ctx, level) {
		return
	}
	_ = h.Handle(ctx, slog.NewRecord(at, level, msg, 0))
}
