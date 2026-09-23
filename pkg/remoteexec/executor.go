package remoteexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	// Env is added to every job, and beats the request's own value for a name
	// they both set.
	//
	// It exists because the request's environment cannot be trusted to carry
	// the interposer. The PostgreSQL shim removes LD_PRELOAD from Plex's
	// environment once it has loaded, so that Plex's ordinary helper children
	// do not inherit a musl-linked library, and re-injects it only when it
	// recognises Plex exec'ing a scanner itself. It never sees ours: the
	// binary Plex execs is the shim, which forwards the call here, and the
	// real one is started by this process instead.
	//
	// Without it a helper opens the local SQLite file rather than the shared
	// library, finds nothing there and exits successfully having done nothing.
	Env    []string
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
	cmd.Env = envList(req.GetEnv(), e.Env)
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

// envList turns the request environment into KEY=VALUE pairs, with extra
// overriding any name the request also sets. An empty request and no extra
// inherits the manager's environment.
//
// A name is never emitted twice. Two values for one name is not an override:
// which one a process sees is down to whichever its libc finds first.
func envList(env map[string]string, extra []string) []string {
	if len(env) == 0 && len(extra) == 0 {
		return nil
	}
	merged := make(map[string]string, len(env)+len(extra))
	if len(env) == 0 {
		// Preserve "empty request inherits the manager's environment", which
		// extra then adds to rather than replaces.
		for _, e := range os.Environ() {
			if k, v, ok := strings.Cut(e, "="); ok {
				merged[k] = v
			}
		}
	}
	maps.Copy(merged, env)
	for _, e := range extra {
		if k, v, ok := strings.Cut(e, "="); ok {
			merged[k] = v
		}
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	list := make([]string, 0, len(keys))
	for _, k := range keys {
		list = append(list, k+"="+merged[k])
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
