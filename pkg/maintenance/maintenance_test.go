package maintenance

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type call struct {
	Member string
	Method string
	Path   string
}

type recorder struct {
	mu    sync.Mutex
	calls []call
	fail  map[string]error
}

func (r *recorder) dispatch(_ context.Context, member, method, path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call{member, method, path})
	return r.fail[member]
}

func (r *recorder) sorted() []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]call(nil), r.calls...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func fanout(rec *recorder, libraries, members []string) *Fanout {
	return &Fanout{
		Libraries: func(context.Context) ([]string, error) { return libraries, nil },
		Members:   func(context.Context) ([]string, error) { return members, nil },
		Dispatch:  rec.dispatch,
	}
}

func TestPerLibraryTaskIsSplitAcrossPods(t *testing.T) {
	// The point of the whole exercise: one pod must not analyse every library.
	rec := &recorder{}
	f := fanout(rec, []string{"1", "2", "3", "4", "5", "6"}, []string{"plex-0", "plex-1", "plex-2"})

	result, err := f.Run(context.Background(), Tasks["analyze"])
	require.NoError(t, err)

	assert.Len(t, result.Dispatched, 6)
	owners := map[string]bool{}
	for _, c := range rec.sorted() {
		owners[c.Member] = true
		assert.Equal(t, "PUT", c.Method)
		assert.Contains(t, c.Path, "/analyze")
	}
	assert.Greater(t, len(owners), 1, "work should reach more than one pod")
}

func TestEachLibraryGoesToExactlyOnePod(t *testing.T) {
	rec := &recorder{}
	f := fanout(rec, []string{"1", "2", "3"}, []string{"plex-0", "plex-1"})

	_, err := f.Run(context.Background(), Tasks["analyze"])
	require.NoError(t, err)

	seen := map[string]int{}
	for _, c := range rec.sorted() {
		seen[c.Path]++
	}
	for path, n := range seen {
		assert.Equal(t, 1, n, "library %s was dispatched more than once", path)
	}
}

func TestTheSameLibraryAlwaysGoesToTheSamePod(t *testing.T) {
	first := &recorder{}
	second := &recorder{}
	libraries, members := []string{"1", "2", "3", "4"}, []string{"plex-0", "plex-1", "plex-2"}

	_, err := fanout(first, libraries, members).Run(context.Background(), Tasks["analyze"])
	require.NoError(t, err)
	_, err = fanout(second, libraries, members).Run(context.Background(), Tasks["analyze"])
	require.NoError(t, err)

	assert.Equal(t, first.sorted(), second.sorted())
}

func TestAServerWideTaskRunsOnOnePodOnly(t *testing.T) {
	// Backing up the database on every pod at once would be pointless at best.
	rec := &recorder{}
	f := fanout(rec, []string{"1", "2", "3"}, []string{"plex-0", "plex-1", "plex-2"})

	result, err := f.Run(context.Background(), Tasks["backup-database"])
	require.NoError(t, err)

	require.Len(t, rec.sorted(), 1)
	assert.Equal(t, "POST", rec.sorted()[0].Method)
	assert.Equal(t, "/butler/BackupDatabase", rec.sorted()[0].Path)
	assert.Len(t, result.Dispatched, 1)
}

func TestAFailureOnOnePodDoesNotStopTheOthers(t *testing.T) {
	// The caller is a Kubernetes Job, so a partial failure has to be reported
	// rather than swallowed, but the rest of the work should still be done.
	rec := &recorder{fail: map[string]error{"plex-0": errors.New("connection refused")}}
	f := fanout(rec, []string{"1", "2", "3", "4", "5", "6"}, []string{"plex-0", "plex-1", "plex-2"})

	result, err := f.Run(context.Background(), Tasks["analyze"])

	require.Error(t, err, "a partial failure must be visible to the Job")
	assert.NotEmpty(t, result.Dispatched)
	assert.NotEmpty(t, result.Failed)
	assert.Len(t, rec.sorted(), 6, "every library is still attempted")
}

func TestRunFailsWhenNoPodIsAvailable(t *testing.T) {
	rec := &recorder{}
	f := fanout(rec, []string{"1"}, nil)

	_, err := f.Run(context.Background(), Tasks["analyze"])

	require.Error(t, err)
	assert.Empty(t, rec.sorted())
}

func TestRunIsANoOpWhenThereAreNoLibraries(t *testing.T) {
	rec := &recorder{}
	f := fanout(rec, nil, []string{"plex-0"})

	result, err := f.Run(context.Background(), Tasks["analyze"])
	require.NoError(t, err)

	assert.Empty(t, result.Dispatched)
	assert.Empty(t, rec.sorted())
}

func TestEveryDisabledButlerTaskHasAReplacement(t *testing.T) {
	// Turning Plex's scheduler off only makes sense if something else runs the
	// work. A task with no entry here would silently stop happening.
	for _, name := range []string{
		"ButlerTaskAnalyzeMedia",
		"ButlerTaskBackupDatabase",
		"ButlerTaskCleanOldBundles",
		"ButlerTaskCleanOldCacheFiles",
		"ButlerTaskDeepMediaAnalysis",
		// ButlerTaskGenerateAutoTags is not here: Plex runs no such task now
		// (GET /butler lists none), so its preference schedules nothing.
		"ButlerTaskGenerateChapterThumbs",
		"ButlerTaskGenerateMediaIndexFiles",
		"ButlerTaskRefreshLocalMedia",
		"ButlerTaskRefreshPeriodicMetadata",
		"ButlerTaskUpgradeMediaAnalysis",
	} {
		assert.True(t, Replaces(name), "no scheduled replacement for %s", name)
	}
}

func TestLookupRejectsAnUnknownTask(t *testing.T) {
	_, ok := Lookup("no-such-task")
	assert.False(t, ok)
}

func TestLookupFindsAKnownTask(t *testing.T) {
	task, ok := Lookup("analyze")
	require.True(t, ok)
	assert.True(t, task.PerLibrary)
}

// plexButlerTasks is GET /butler's task names, as kind-cluster-plex's Plex
// answered it on 2026-10-06. POST /butler/{name} runs one; the preference
// that schedules it (ButlerTaskCleanOldCacheFiles) is not its name, and
// Plex answers 404 to it -- which every server-wide maintenance task got
// until then, failing its CronJob.
var plexButlerTasks = []string{
	"AutomaticUpdates", "BackupDatabase", "ButlerTaskCleanSupplementalLogFiles",
	"ButlerTaskGenerateAdMarkers", "ButlerTaskGenerateCreditsMarkers",
	"ButlerTaskGenerateIntroMarkers", "ButlerTaskGenerateVoiceActivity",
	"CleanOldBundles", "CleanOldCacheFiles", "DeepMediaAnalysis",
	"GarbageCollectBlobs", "GarbageCollectLibraryMedia", "GenerateChapterThumbs",
	"GenerateMediaIndexFiles", "LoudnessAnalysis", "MusicAnalysis",
	"OptimizeDatabase", "ProcessAssets", "RefreshEpgGuides", "RefreshLibraries",
	"RefreshLocalMedia", "RefreshPeriodicMetadata", "UpgradeMediaAnalysis",
}

func TestEveryButlerTaskNamesOnePlexRuns(t *testing.T) {
	for name, task := range Tasks {
		butler, ok := strings.CutPrefix(task.Path, "/butler/")
		if !ok {
			continue
		}
		assert.Contains(t, plexButlerTasks, butler, "%s posts to %s, which Plex answers 404", name, task.Path)
	}
}

// deadlineOf runs task with a dispatch that records how long its context gives
// it, measured from just before Run.
func deadlineOf(t *testing.T, task Task) time.Duration {
	t.Helper()
	var got time.Duration
	var mu sync.Mutex
	f := &Fanout{
		Libraries: func(context.Context) ([]string, error) { return []string{"1"}, nil },
		Members:   func(context.Context) ([]string, error) { return []string{"a"}, nil },
		Dispatch: func(ctx context.Context, _, _, _ string) error {
			deadline, ok := ctx.Deadline()
			if !assert.True(t, ok, "a dispatch must not run unbounded") {
				return nil
			}
			mu.Lock()
			got = time.Until(deadline)
			mu.Unlock()
			return nil
		},
	}
	_, err := f.Run(context.Background(), task)
	require.NoError(t, err)
	return got
}

func TestATaskPlexOnlyQueuesHasTheDefaultDeadline(t *testing.T) {
	// Plex answers an analyze or a refresh once it has queued the work, so a
	// pod that has not answered in DefaultTimeout is not going to.
	got := deadlineOf(t, Tasks["refresh"])
	assert.InDelta(t, DefaultTimeout.Seconds(), got.Seconds(), 1)
}

func TestEmptyTrashHasTimeToEmptyIt(t *testing.T) {
	// Plex answers PUT /library/sections/{id}/emptyTrash only once the trash
	// is gone: 6 s for talos's TV library with nothing in it, and more than a
	// minute after a few shows were deleted (2026-10-22). Under the 30 s every
	// task had, each run's first one or two attempts failed with "context
	// deadline exceeded" and only a retry, after Plex had finished on its own,
	// passed.
	got := deadlineOf(t, Tasks["empty-trash"])
	assert.Greater(t, got, 29*time.Minute)
}
