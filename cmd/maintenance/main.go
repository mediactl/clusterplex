// The maintenance command asks a manager to distribute one background task.
//
// It is what a Kubernetes CronJob runs. Keeping the schedule in Kubernetes
// rather than in a timer inside the manager is the whole point: a call that
// fails during a failover becomes a failed Job that retries and shows up in
// kubectl, instead of an error swallowed in a goroutine.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mediactl/clusterplex/pkg/maintenance"
)

// waitFor is how long the trigger waits for the manager: timeout when one is
// given, else a minute past the task's own deadline, so the Job never gives up
// while the manager is still working and its retry never asks Plex for the
// same work a second time.
func waitFor(task string, timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	if t, ok := maintenance.Lookup(task); ok {
		return t.Deadline() + time.Minute
	}
	// The manager answers an unknown task 404 at once.
	return time.Minute
}

func main() {
	endpoint := flag.String("endpoint", "", "base URL of a manager, for example http://plex-workers:8080")
	task := flag.String("task", "", "the maintenance task to distribute")
	timeout := flag.Duration("timeout", 0, "how long to wait for the manager to accept; 0 waits a minute past the task's own deadline")
	flag.Parse()

	if *endpoint == "" || *task == "" {
		fmt.Fprintln(os.Stderr, "both -endpoint and -task are required")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	code := run(ctx, *endpoint, *task, waitFor(*task, *timeout), os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run asks the manager to distribute the task, returning the process exit code.
// Anything other than success exits non-zero so the Job fails and is retried.
func run(ctx context.Context, endpoint, task string, timeout time.Duration, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := strings.TrimSuffix(endpoint, "/") + "/api/v1/maintenance/" + task
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "maintenance %s: %v\n", task, err)
		return 1
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Most likely a failover in progress. Failing is correct: the Job
		// retries, by which time a new pod has taken over.
		_, _ = fmt.Fprintf(stderr, "maintenance %s: %v\n", task, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode != http.StatusAccepted {
		_, _ = fmt.Fprintf(stderr, "maintenance %s: %s: %s\n", task, resp.Status, strings.TrimSpace(string(body)))
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "maintenance %s accepted: %s\n", task, strings.TrimSpace(string(body)))
	return 0
}
