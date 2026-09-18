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
)

func main() {
	endpoint := flag.String("endpoint", "", "base URL of a manager, for example http://plex-workers:8080")
	task := flag.String("task", "", "the maintenance task to distribute")
	timeout := flag.Duration("timeout", time.Minute, "how long to wait for the manager to accept")
	flag.Parse()

	if *endpoint == "" || *task == "" {
		fmt.Fprintln(os.Stderr, "both -endpoint and -task are required")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	code := run(ctx, *endpoint, *task, *timeout, os.Stdout, os.Stderr)
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
