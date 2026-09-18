package remoteexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	pb "github.com/mediactl/clusterplex/proto"
)

// Sink receives the streamed output of a job. A gRPC server stream satisfies it.
type Sink interface {
	Send(*pb.TranscodeLog) error
}

const (
	// DefaultSuffix is appended to a shimmed binary's name to find the real one.
	DefaultSuffix = ".real"
	// termGrace is how long a cancelled job gets between SIGTERM and SIGKILL.
	termGrace = 5 * time.Second
)

// Executor runs the real Plex binaries on the local node and streams their
// output to a Sink.
type Executor struct {
	// BinDir holds the real binaries, e.g. /usr/lib/plexmediaserver.
	BinDir string
	// Suffix is the real binary's suffix; DefaultSuffix when empty.
	Suffix string
	Logger *slog.Logger
}

// Binary resolves target to the real binary's path. Names containing path
// separators are rejected so a caller cannot escape BinDir.
func (e *Executor) Binary(target string) (string, error) {
	if target == "" || target != filepath.Base(target) || strings.ContainsAny(target, `/\`) {
		return "", fmt.Errorf("invalid target binary %q", target)
	}
	suffix := e.Suffix
	if suffix == "" {
		suffix = DefaultSuffix
	}
	p := filepath.Join(e.BinDir, target+suffix)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("target binary %q: %w", target, err)
	}
	return p, nil
}

// Run executes the request and streams stdout, stderr and the exit status to
// sink. The last message carries IsFinished and the exit code. Cancelling ctx
// terminates the child's whole process group, so helpers it spawned die too.
// Run only returns an error when the process cannot start or the sink fails.
func (e *Executor) Run(ctx context.Context, req *pb.ExecRequest, sink Sink) error {
	bin, err := e.Binary(req.GetTargetBinary())
	if err != nil {
		return err
	}

	out := &sinkWriter{sink: &lockedSink{sink: sink}, stderr: false}
	errw := &sinkWriter{sink: out.sink, stderr: true}

	cmd := exec.CommandContext(ctx, bin, req.GetArgs()...)
	cmd.Dir = req.GetCwd()
	cmd.Env = envList(req.GetEnv())
	cmd.Stdout = out
	cmd.Stderr = errw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return signalGroup(cmd, syscall.SIGTERM) }
	cmd.WaitDelay = termGrace

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %q: %w", bin, err)
	}
	e.log().Info("job started", "target", req.GetTargetBinary(), "pid", cmd.Process.Pid)

	waitErr := cmd.Wait()
	_ = signalGroup(cmd, syscall.SIGKILL) // reap helpers that outlived their parent

	code := exitCode(waitErr)
	e.log().Info("job finished", "target", req.GetTargetBinary(), "exit_code", code)
	out.sink.send(&pb.TranscodeLog{IsFinished: true, ExitCode: code})
	if err := out.sink.err(); err != nil {
		return fmt.Errorf("stream output: %w", err)
	}
	return nil
}

func (e *Executor) log() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

// envList turns the request environment into KEY=VALUE pairs. An empty map
// inherits the manager's environment.
func envList(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	list := make([]string, 0, len(keys))
	for _, k := range keys {
		list = append(list, k+"="+env[k])
	}
	return list
}

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// exitCode maps Wait's result to what the shim should exit with. A process
// killed by a signal reports -1, which the shim passes through as failure.
func exitCode(waitErr error) int32 {
	if waitErr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return int32(ee.ExitCode())
	}
	return -1
}

// lockedSink serialises Send calls, which a gRPC stream does not allow
// concurrently, and remembers the first failure.
type lockedSink struct {
	mu   sync.Mutex
	sink Sink
	fail error
}

func (l *lockedSink) send(m *pb.TranscodeLog) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail == nil {
		l.fail = l.sink.Send(m)
	}
}

func (l *lockedSink) err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fail
}

// sinkWriter turns each Write into one streamed chunk.
type sinkWriter struct {
	sink   *lockedSink
	stderr bool
}

func (w *sinkWriter) Write(p []byte) (int, error) {
	chunk := append([]byte(nil), p...)
	if w.stderr {
		w.sink.send(&pb.TranscodeLog{StderrChunk: chunk})
	} else {
		w.sink.send(&pb.TranscodeLog{StdoutChunk: chunk})
	}
	return len(p), nil
}
