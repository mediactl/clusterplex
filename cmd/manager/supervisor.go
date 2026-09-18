package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

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
	Logger *slog.Logger
	// PIDFile is Plex's pid file. Plex refuses to start when it names a live
	// process; on a persistent volume the file outlives the container and
	// container pids repeat, so it is removed before every start.
	PIDFile string
	// Grace is how long Plex gets after SIGTERM before it is killed.
	Grace time.Duration
	// Redirect, when set, runs before Plex starts. A failure is logged, not
	// fatal: the Service still reaches the proxy port directly.
	Redirect func(ctx context.Context) error
	// Proxy, when set, starts before Plex and stops after it.
	Proxy *proxy.TCP
	// OnUnexpectedExit is called when Plex exits without Stop having been called.
	OnUnexpectedExit func(err error)

	mu       sync.Mutex
	cmd      *exec.Cmd
	stopping bool
	done     chan struct{}
	cancel   context.CancelFunc
}

// Start applies the redirect, starts the proxy and then Plex.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		return errors.New("plex media server is already running")
	}

	if s.Redirect != nil {
		if err := s.Redirect(ctx); err != nil {
			s.Logger.Error("port redirect failed; Plex is reachable only through the proxy port", "error", err)
		}
	}

	pctx, cancel := context.WithCancel(ctx)
	if s.Proxy != nil {
		addr, err := s.Proxy.Start(pctx)
		if err != nil {
			cancel()
			return fmt.Errorf("start proxy: %w", err)
		}
		s.Logger.Info("proxy listening", "addr", addr.String(), "target", s.Proxy.Target)
	}

	s.removeStalePIDFile()
	cmd := exec.Command(s.Binary)
	cmd.Stdout = &lineLogger{log: s.Logger, source: "pms-stdout"}
	cmd.Stderr = &lineLogger{log: s.Logger, source: "pms-stderr"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
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

func (s *Supervisor) removeStalePIDFile() {
	if s.PIDFile == "" {
		return
	}
	err := os.Remove(s.PIDFile)
	switch {
	case err == nil:
		s.Logger.Info("removed stale Plex pid file", "path", s.PIDFile)
	case !errors.Is(err, os.ErrNotExist):
		s.Logger.Warn("remove Plex pid file", "path", s.PIDFile, "error", err)
	}
}

func (s *Supervisor) wait(cmd *exec.Cmd) {
	err := cmd.Wait()
	s.mu.Lock()
	stopping, done := s.stopping, s.done
	s.mu.Unlock()
	close(done)

	if stopping {
		s.Logger.Info("Plex Media Server stopped", "error", err)
		return
	}
	if err == nil {
		err = errors.New("exited with status 0")
	}
	s.Logger.Error("Plex Media Server exited unexpectedly", "error", err)
	if s.OnUnexpectedExit != nil {
		s.OnUnexpectedExit(err)
	}
}

// Stop sends SIGTERM to Plex's process group, waits up to Grace, then kills
// it, and finally stops the proxy. It is a no-op when nothing is running.
func (s *Supervisor) Stop(ctx context.Context) error {
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

	grace := s.Grace
	if grace == 0 {
		grace = defaultGrace
	}
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

// lineLogger relays a process's output to slog one line at a time.
type lineLogger struct {
	log    *slog.Logger
	source string
	mu     sync.Mutex
	buf    []byte
}

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
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	w.log.Info(string(line), "source", w.source)
}
