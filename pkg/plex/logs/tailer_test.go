package plexlogs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/logging"
)

// out collects the records the tailer writes, as JSON lines.
type out struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *out) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *out) records(t *testing.T) []map[string]any {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	var recs []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(o.buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal(line, &m), "%s", line)
		recs = append(recs, m)
	}
	return recs
}

func msgs(recs []map[string]any) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r["msg"].(string))
	}
	return out
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// newTailer is a Tailer over a fresh directory, its records into o, its
// clock c, with its own registry.
func newTailer(t *testing.T, opts logging.Options) (*Tailer, *out, *clock, *prometheus.Registry) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, PluginDir), 0o755))
	o := &out{}
	opts.Output = o
	opts.Format = "json"
	c := &clock{t: time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)}
	reg := prometheus.NewRegistry()
	tl := &Tailer{Dir: dir, Logger: logging.New(opts), Interval: time.Hour, Metrics: NewMetrics(reg), Now: c.now}
	return tl, o, c, reg
}

const pmsName = "Plex Media Server.log"

func write(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(s)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func stamp(level, msg string) string {
	return "Nov 10, 2026 18:20:17.104 [1] " + level + " - " + msg + "\n"
}

func TestALineIsOneRecordWithPlexsTime(t *testing.T) {
	tl, o, _, _ := newTailer(t, logging.Options{})
	write(t, filepath.Join(tl.Dir, pmsName), stamp("INFO", "Plex Media Server v1.43 started"))
	require.NoError(t, tl.tick(t.Context(), false))
	recs := o.records(t)
	require.Len(t, recs, 1)
	assert.Equal(t, "2026-11-10T18:20:17.104Z", recs[0]["time"])
	assert.Equal(t, "INFO", recs[0]["level"])
	assert.Equal(t, "pms", recs[0]["subsystem"])
	assert.Equal(t, "1", recs[0]["thread"])
	assert.Equal(t, pmsName, recs[0]["file"])
	assert.Equal(t, "Plex Media Server v1.43 started", recs[0]["msg"])
	assert.Nil(t, recs[0]["req"], "no request id, no key")
}

func TestARotatedFileIsReadToItsEndThenDeleted(t *testing.T) {
	tl, o, _, _ := newTailer(t, logging.Options{})
	live := filepath.Join(tl.Dir, pmsName)
	write(t, live, stamp("INFO", "one"))
	require.NoError(t, tl.tick(t.Context(), false))
	write(t, live, stamp("INFO", "two"))
	// Plex rotates: the live file becomes .1.log and a new one takes the name.
	rotated := filepath.Join(tl.Dir, "Plex Media Server.1.log")
	require.NoError(t, os.Rename(live, rotated))
	write(t, live, stamp("INFO", "three"))
	require.NoError(t, tl.tick(t.Context(), false))
	assert.Equal(t, []string{"one", "two", "three"}, msgs(o.records(t)))
	_, err := os.Stat(rotated)
	assert.True(t, os.IsNotExist(err), "the rotated file is deleted once read")
	_, err = os.Stat(live)
	assert.NoError(t, err, "the live file stays")
	assert.Equal(t, 1.0, testutil.ToFloat64(tl.Metrics.deleted.WithLabelValues(pmsName)))
}

func TestARotationMidLineLosesAndDoublesNothing(t *testing.T) {
	tl, o, _, _ := newTailer(t, logging.Options{})
	live := filepath.Join(tl.Dir, pmsName)
	write(t, live, stamp("INFO", "whole")+"Nov 10, 2026 18:20:18.000 [1] INFO - cut sho")
	require.NoError(t, tl.tick(t.Context(), false))
	write(t, live, "rt\n")
	require.NoError(t, os.Rename(live, filepath.Join(tl.Dir, "Plex Media Server.1.log")))
	write(t, live, stamp("INFO", "fresh"))
	require.NoError(t, tl.tick(t.Context(), false))
	assert.Equal(t, []string{"whole", "cut short", "fresh"}, msgs(o.records(t)))
}

func TestATruncatedLiveFileIsReadFromItsStart(t *testing.T) {
	tl, o, _, _ := newTailer(t, logging.Options{})
	live := filepath.Join(tl.Dir, pmsName)
	write(t, live, stamp("INFO", "a long first line that will be gone")+stamp("INFO", "second"))
	require.NoError(t, tl.tick(t.Context(), false))
	require.NoError(t, os.WriteFile(live, []byte(stamp("INFO", "new")), 0o644))
	require.NoError(t, tl.tick(t.Context(), false))
	assert.Equal(t, []string{"a long first line that will be gone", "second", "new"}, msgs(o.records(t)))
}

func TestFilesPresentAtStartAreDrainedAndRotatedOnesRemoved(t *testing.T) {
	tl, o, _, _ := newTailer(t, logging.Options{Level: slog.LevelDebug}) // the plugin's line is DEBUG
	rotated := filepath.Join(tl.Dir, "Plex Media Server.1.log")
	live := filepath.Join(tl.Dir, pmsName)
	write(t, rotated, stamp("INFO", "old"))
	write(t, live, stamp("INFO", "current"))
	plugin := filepath.Join(tl.Dir, PluginDir, "com.plexapp.system.log")
	write(t, plugin, "2026-11-10 18:20:21,343 (ffff893ef890) :  DEBUG (core:349) - Starting framework core\n")
	require.NoError(t, tl.Drain(t.Context()))
	recs := o.records(t)
	assert.ElementsMatch(t, []string{"old", "current", "(core:349) - Starting framework core"}, msgs(recs))
	for _, r := range recs {
		if r["subsystem"] == "pms.plugin" {
			assert.Equal(t, "com.plexapp.system", r["plugin"])
			assert.Equal(t, "DEBUG", r["level"])
		}
	}
	_, err := os.Stat(rotated)
	assert.True(t, os.IsNotExist(err), "the rotated file is deleted once drained")
	_, err = os.Stat(live)
	assert.NoError(t, err, "the live file stays")
}

func TestAStaleLiveFileIsDeleted(t *testing.T) {
	tl, o, c, _ := newTailer(t, logging.Options{})
	scanner := filepath.Join(tl.Dir, "Plex Media Scanner.log")
	write(t, scanner, stamp("INFO", "scanned"))
	require.NoError(t, tl.tick(t.Context(), true))
	_, err := os.Stat(scanner)
	assert.NoError(t, err, "a file written within StaleAfter stays")
	c.add(25 * time.Hour)
	require.NoError(t, tl.tick(t.Context(), true))
	_, err = os.Stat(scanner)
	assert.True(t, os.IsNotExist(err), "a live file nothing wrote for 24 h is deleted")
	assert.Equal(t, []string{"scanned"}, msgs(o.records(t)))
}

func TestFallingBehindWarnsOnce(t *testing.T) {
	tl, o, _, _ := newTailer(t, logging.Options{})
	tl.BehindWarn = 1 << 10
	live := filepath.Join(tl.Dir, pmsName)
	big := strings.Repeat("x", 4<<10)
	write(t, live, stamp("INFO", big))
	require.NoError(t, tl.tick(t.Context(), false))
	write(t, live, stamp("INFO", big))
	require.NoError(t, tl.tick(t.Context(), false))
	var warns []map[string]any
	for _, r := range o.records(t) {
		if r["subsystem"] == "supervisor" {
			warns = append(warns, r)
		}
	}
	require.Len(t, warns, 1, "one WARN for the episode")
	assert.Equal(t, "WARN", warns[0]["level"])
	assert.Greater(t, warns[0]["bytes"], float64(1<<10))
}

func TestAnOverrideQuietsPlex(t *testing.T) {
	tl, o, _, _ := newTailer(t, logging.Options{Subsystems: map[string]slog.Level{"pms": slog.LevelWarn}})
	write(t, filepath.Join(tl.Dir, pmsName), stamp("INFO", "chatter")+stamp("ERROR", "broken"))
	require.NoError(t, tl.tick(t.Context(), false))
	assert.Equal(t, []string{"broken"}, msgs(o.records(t)))
}

func TestTheMetricsCount(t *testing.T) {
	tl, _, _, _ := newTailer(t, logging.Options{})
	line := stamp("ERROR", "broken")
	write(t, filepath.Join(tl.Dir, pmsName), line+line)
	require.NoError(t, tl.tick(t.Context(), false))
	assert.Equal(t, 2.0, testutil.ToFloat64(tl.Metrics.lines.WithLabelValues("pms", "ERROR")))
	assert.Equal(t, float64(2*len(line)), testutil.ToFloat64(tl.Metrics.bytes.WithLabelValues(pmsName)))
}

func TestRunRefusesANilLogger(t *testing.T) {
	assert.ErrorIs(t, (&Tailer{Dir: t.TempDir()}).Run(t.Context()), errNoLogger)
}

func TestRunFollowsUntilTheContextEnds(t *testing.T) {
	tl, o, _, _ := newTailer(t, logging.Options{})
	tl.Interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- tl.Run(ctx) }()
	live := filepath.Join(tl.Dir, pmsName)
	write(t, live, stamp("INFO", "first"))
	assert.Eventually(t, func() bool { return len(o.records(t)) == 1 }, 2*time.Second, 5*time.Millisecond)
	write(t, live, stamp("INFO", "last"))
	cancel()
	require.NoError(t, <-done)
	assert.Equal(t, []string{"first", "last"}, msgs(o.records(t)), "the last drain reads what came before the stop")
}
