// Package maintenance runs the background work Plex's own scheduler used to do.
//
// Plex's scheduler is disabled on every pod, because each process runs its own
// copy with no knowledge of the others and they would all analyse the same
// media at once. This replaces it: a Kubernetes CronJob calls the manager, the
// manager decides which pod owns each library, and asks that pod to do the work.
//
// Scheduling deliberately lives outside this process. A CronJob that cannot
// reach the manager during a failover produces a failed Job and a retry, which
// is visible; a timer inside a long-running daemon produces a swallowed error
// in a goroutine, which is not.
package maintenance

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/mediactl/clusterplex/pkg/hashring"
)

// Task is a unit of maintenance and how to ask Plex to do it.
type Task struct {
	// Name is what a CronJob calls.
	Name   string
	Method string
	// Path addresses the work. For a per-library task it contains {id}.
	Path string
	// PerLibrary spreads the task across pods by library. Otherwise it runs
	// once, on one pod.
	PerLibrary bool
	// Butler names the Plex scheduled task this replaces, for the check that
	// nothing was disabled without a replacement.
	Butler []string
}

// Tasks is what a CronJob may ask for.
//
// The per-library ones address Plex's library endpoints so the work divides.
// The rest go through Plex's own butler endpoint, which runs a named scheduled
// task on demand: those are server-wide, so spreading them would only mean
// doing the same work several times.
var Tasks = map[string]Task{
	"analyze": {
		Name: "analyze", Method: "PUT", Path: "/library/sections/{id}/analyze", PerLibrary: true,
		Butler: []string{"ButlerTaskAnalyzeMedia", "ButlerTaskUpgradeMediaAnalysis"},
	},
	"deep-analyze": {
		Name: "deep-analyze", Method: "PUT", Path: "/library/sections/{id}/analyze?deep=1", PerLibrary: true,
		Butler: []string{"ButlerTaskDeepMediaAnalysis"},
	},
	"refresh": {
		Name: "refresh", Method: "GET", Path: "/library/sections/{id}/refresh", PerLibrary: true,
		Butler: []string{"ButlerTaskRefreshLocalMedia", "ButlerTaskRefreshPeriodicMetadata"},
	},
	"empty-trash": {
		Name: "empty-trash", Method: "PUT", Path: "/library/sections/{id}/emptyTrash", PerLibrary: true,
	},
	"media-index": {
		Name: "media-index", Method: "POST", Path: "/butler/ButlerTaskGenerateMediaIndexFiles",
		Butler: []string{"ButlerTaskGenerateMediaIndexFiles"},
	},
	"chapter-thumbs": {
		Name: "chapter-thumbs", Method: "POST", Path: "/butler/ButlerTaskGenerateChapterThumbs",
		Butler: []string{"ButlerTaskGenerateChapterThumbs"},
	},
	"auto-tags": {
		Name: "auto-tags", Method: "POST", Path: "/butler/ButlerTaskGenerateAutoTags",
		Butler: []string{"ButlerTaskGenerateAutoTags"},
	},
	"backup-database": {
		Name: "backup-database", Method: "POST", Path: "/butler/ButlerTaskBackupDatabase",
		Butler: []string{"ButlerTaskBackupDatabase"},
	},
	"clean-bundles": {
		Name: "clean-bundles", Method: "POST", Path: "/butler/ButlerTaskCleanOldBundles",
		Butler: []string{"ButlerTaskCleanOldBundles"},
	},
	"clean-cache": {
		Name: "clean-cache", Method: "POST", Path: "/butler/ButlerTaskCleanOldCacheFiles",
		Butler: []string{"ButlerTaskCleanOldCacheFiles"},
	},
}

// Lookup finds a task by the name a CronJob uses.
func Lookup(name string) (Task, bool) {
	t, ok := Tasks[name]
	return t, ok
}

// Replaces reports whether some task here covers a disabled Plex scheduled
// task. Disabling one without a replacement means the work stops happening.
func Replaces(butlerTask string) bool {
	for _, t := range Tasks {
		if slices.Contains(t.Butler, butlerTask) {
			return true
		}
	}
	return false
}

// Names lists the tasks a CronJob may ask for, sorted.
func Names() []string {
	out := make([]string, 0, len(Tasks))
	for name := range Tasks {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Assignment records where one piece of work went.
type Assignment struct {
	Member  string
	Path    string
	Library string
}

// Result is what happened, for the caller to log and for the Job to fail on.
type Result struct {
	Task       string
	Dispatched []Assignment
	Failed     []Assignment
}

// Fanout distributes a task across the pods currently running Plex.
type Fanout struct {
	// Libraries lists the library section identifiers.
	Libraries func(ctx context.Context) ([]string, error)
	// Members lists the pods that can do the work.
	Members func(ctx context.Context) ([]string, error)
	// Dispatch asks one pod to do one piece of work.
	Dispatch func(ctx context.Context, member, method, path string) error
	Logger   *slog.Logger
}

// Run distributes task and reports what happened. It returns an error when any
// piece failed, having still attempted all of them: the caller is a Job, and a
// partial failure should be visible rather than swallowed.
func (f *Fanout) Run(ctx context.Context, task Task) (Result, error) {
	result := Result{Task: task.Name}

	members, err := f.Members(ctx)
	if err != nil {
		return result, fmt.Errorf("list pods: %w", err)
	}
	ring := hashring.New(members...)
	if len(ring.Members()) == 0 {
		return result, fmt.Errorf("%s: no pod is running Plex", task.Name)
	}

	work, err := f.plan(ctx, task, ring)
	if err != nil {
		return result, err
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, item := range work {
		wg.Add(1)
		go func(item Assignment) {
			defer wg.Done()
			err := f.Dispatch(ctx, item.Member, task.Method, item.Path)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				f.log().Error("maintenance task failed",
					"task", task.Name, "pod", item.Member, "path", item.Path, "error", err)
				result.Failed = append(result.Failed, item)
				return
			}
			result.Dispatched = append(result.Dispatched, item)
		}(item)
	}
	wg.Wait()

	if len(result.Failed) > 0 {
		return result, fmt.Errorf("%s: %d of %d dispatches failed", task.Name, len(result.Failed), len(work))
	}
	return result, nil
}

// plan decides what to send where, without sending anything.
func (f *Fanout) plan(ctx context.Context, task Task, ring *hashring.Ring) ([]Assignment, error) {
	if !task.PerLibrary {
		// Server-wide work runs once. Hashing the task name rather than always
		// picking the first pod keeps different tasks on different pods.
		return []Assignment{{Member: ring.Locate(task.Name), Path: task.Path}}, nil
	}

	libraries, err := f.Libraries(ctx)
	if err != nil {
		return nil, fmt.Errorf("list libraries: %w", err)
	}
	work := make([]Assignment, 0, len(libraries))
	for _, id := range libraries {
		work = append(work, Assignment{
			Member:  ring.Locate(id),
			Path:    strings.ReplaceAll(task.Path, "{id}", id),
			Library: id,
		})
	}
	return work, nil
}

func (f *Fanout) log() *slog.Logger {
	if f.Logger != nil {
		return f.Logger
	}
	return slog.Default()
}
