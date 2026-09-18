// Package portredirect steers inbound connections for one port to another
// inside the pod's network namespace. Plex Media Server always binds
// 0.0.0.0:32400 and offers no setting to change it, so the only way to put a
// proxy in front of it without a second network namespace is a nat REDIRECT.
package portredirect

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Runner executes a command. It exists so tests can observe the iptables calls.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
}

// ExecRunner runs commands with os/exec and folds their output into the error.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func rule(op string, from, to int) []string {
	return []string{
		"-w", "5", "-t", "nat", op, "PREROUTING",
		"-p", "tcp", "--dport", strconv.Itoa(from),
		"-j", "REDIRECT", "--to-ports", strconv.Itoa(to),
	}
}

// Ensure makes TCP connections entering the namespace for port from land on
// port to. Connections that originate inside the pod, such as the proxy
// dialling loopback or Plex's own helpers, traverse OUTPUT rather than
// PREROUTING and are left alone. Ensure is idempotent.
func Ensure(ctx context.Context, r Runner, from, to int) error {
	if err := r.Run(ctx, "iptables", rule("-C", from, to)...); err == nil {
		return nil
	}
	if err := r.Run(ctx, "iptables", rule("-A", from, to)...); err != nil {
		return fmt.Errorf("add redirect %d->%d: %w", from, to, err)
	}
	return nil
}
