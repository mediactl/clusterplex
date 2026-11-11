package logging

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

func changedSet(names ...string) func(string) bool {
	return func(n string) bool { return slices.Contains(names, n) }
}

// syncBuffer is a bytes.Buffer the watcher's goroutine and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// The file wins, then a flag that was set, then the environment, then the
// defaults (design §4); the file's subsystems replace the flags' whole.
func TestResolvePrecedence(t *testing.T) {
	flags := Options{Level: slog.LevelDebug, Format: "text", Subsystems: map[string]slog.Level{"a": slog.LevelError}}
	cases := []struct {
		name     string
		file     FileConfig
		haveFile bool
		changed  []string
		env      map[string]string
		level    slog.Level
		format   string
		subs     map[string]slog.Level
	}{
		{"file beats flag beats env", FileConfig{Level: "warn"}, true, []string{"log-level"}, map[string]string{"LOG_LEVEL": "error"}, slog.LevelWarn, "json", map[string]slog.Level{"a": slog.LevelError}},
		{"flag beats env", FileConfig{}, false, []string{"log-level", "log-format"}, map[string]string{"LOG_LEVEL": "error", "LOG_FORMAT": "json"}, slog.LevelDebug, "text", map[string]slog.Level{"a": slog.LevelError}},
		{"an unchanged flag loses to the env", FileConfig{}, false, nil, map[string]string{"LOG_LEVEL": "error", "LOG_FORMAT": "text"}, slog.LevelError, "text", map[string]slog.Level{"a": slog.LevelError}},
		{"nothing set is info and json", FileConfig{}, false, nil, nil, slog.LevelInfo, "json", map[string]slog.Level{"a": slog.LevelError}},
		{"the file's subsystems replace the flags'", FileConfig{Subsystems: map[string]string{"pms": "trace"}}, true, []string{"log-level"}, nil, slog.LevelDebug, "json", map[string]slog.Level{"pms": LevelTrace}},
		{"the file's format wins at start", FileConfig{Format: "text"}, true, nil, map[string]string{"LOG_FORMAT": "json"}, slog.LevelInfo, "text", map[string]slog.Level{"a": slog.LevelError}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.file, tc.haveFile, flags, changedSet(tc.changed...), envOf(tc.env))
			require.NoError(t, err)
			require.Equal(t, tc.level, got.Level)
			require.Equal(t, tc.format, got.Format)
			require.Equal(t, tc.subs, got.Subsystems)
		})
	}
	_, err := Resolve(FileConfig{Level: "loud"}, true, flags, changedSet(), envOf(nil))
	require.ErrorContains(t, err, "level")
	_, err = Resolve(FileConfig{Subsystems: map[string]string{"pms": "loud"}}, true, flags, changedSet(), envOf(nil))
	require.ErrorContains(t, err, "pms")
}

func TestReadConfigReadsTheLoggingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("plex:\n  dir: /x\nlogging:\n  level: debug\n  subsystems:\n    pms: warn\n"), 0o644))
	fc, err := ReadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "debug", fc.Level)
	require.Equal(t, map[string]string{"pms": "warn"}, fc.Subsystems)
	require.Nil(t, fc.AddSource)
}

func TestReadConfigOfAMissingFile(t *testing.T) {
	_, err := ReadConfig(filepath.Join(t.TempDir(), "none.yaml"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestBindFlagsTakesSubsystemLevelsAndTheConfigPath(t *testing.T) {
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	var o Options
	BindFlags(fs, &o)
	require.NoError(t, fs.Parse([]string{"--log-subsystem-level=livetv.tuner=debug", "--log-subsystem-level", "pms=fatal", "--log-level=warn"}))
	require.Equal(t, map[string]slog.Level{"livetv.tuner": slog.LevelDebug, "pms": LevelFatal}, o.Subsystems)
	require.Equal(t, "/etc/clusterplex/config.yaml", o.ConfigPath)
	require.True(t, o.Changed("log-level"))
	require.False(t, o.Changed("log-format"))
	require.Error(t, fs.Parse([]string{"--log-subsystem-level=nolevel"}))
	require.Error(t, pflag.NewFlagSet("t", pflag.ContinueOnError).Parse([]string{"--log-subsystem-level=a=loud"}))
}

// A watcher over path at 10 ms, its WARN lines captured.
func startWatch(t *testing.T, path string) (*Levels, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	levels := NewLevels(slog.LevelInfo, nil)
	root := slog.New(NewHandler(buf, "json", false, Service{}, levels))
	ctx, cancel := context.WithCancel(NewContext(context.Background(), root))
	t.Cleanup(cancel)
	go WatchFile(ctx, path, levels, Options{}, changedSet(), envOf(nil), 10*time.Millisecond)
	return levels, buf
}

func writeLevel(t *testing.T, path, level string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte("logging:\n  level: "+level+"\n"), 0o644))
}

func TestWatchFileAppliesAChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logging.yaml")
	writeLevel(t, path, "info")
	levels, _ := startWatch(t, path)
	writeLevel(t, path, "debug")
	require.Eventually(t, func() bool { return levels.For("x") == slog.LevelDebug }, time.Second, 5*time.Millisecond)
}

// The kubelet swaps a ConfigMap's ..data symlink to a new timestamped
// directory; the file's own name never changes.
func TestWatchFileFollowsAConfigMapSymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "..2026_11_10_1"), filepath.Join(dir, "..2026_11_10_2")
	require.NoError(t, os.Mkdir(first, 0o755))
	writeLevel(t, filepath.Join(first, "logging.yaml"), "debug")
	require.NoError(t, os.Symlink("..2026_11_10_1", filepath.Join(dir, "..data")))
	path := filepath.Join(dir, "logging.yaml")
	require.NoError(t, os.Symlink("..data/logging.yaml", path))
	levels, _ := startWatch(t, path)
	require.Eventually(t, func() bool { return levels.For("x") == slog.LevelDebug }, time.Second, 5*time.Millisecond)

	require.NoError(t, os.Mkdir(second, 0o755))
	writeLevel(t, filepath.Join(second, "logging.yaml"), "warn")
	require.NoError(t, os.Symlink("..2026_11_10_2", filepath.Join(dir, "..data_tmp")))
	require.NoError(t, os.Rename(filepath.Join(dir, "..data_tmp"), filepath.Join(dir, "..data")))
	require.Eventually(t, func() bool { return levels.For("x") == slog.LevelWarn }, time.Second, 5*time.Millisecond)
}

func warnLines(buf *syncBuffer) int {
	return strings.Count(buf.String(), `"level":"WARN"`)
}

func TestWatchFileKeepsTheLastGoodSettingsOnABadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logging.yaml")
	writeLevel(t, path, "debug")
	levels, buf := startWatch(t, path)
	require.Eventually(t, func() bool { return levels.For("x") == slog.LevelDebug }, time.Second, 5*time.Millisecond)

	writeLevel(t, path, "[")
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, slog.LevelDebug, levels.For("x"), "the last good settings stay")
	require.Equal(t, 1, warnLines(buf), buf.String())
	require.Contains(t, buf.String(), `"subsystem":"logging"`)
	require.Contains(t, buf.String(), `"err":`)

	writeLevel(t, path, "loud")
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, slog.LevelDebug, levels.For("x"))
	require.Equal(t, 2, warnLines(buf), "one WARN per bad content")

	writeLevel(t, path, "error")
	require.Eventually(t, func() bool { return levels.For("x") == slog.LevelError }, time.Second, 5*time.Millisecond)
}

func TestWatchFileKeepsTheLastGoodSettingsWhenTheFileGoes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logging.yaml")
	writeLevel(t, path, "debug")
	levels, buf := startWatch(t, path)
	require.Eventually(t, func() bool { return levels.For("x") == slog.LevelDebug }, time.Second, 5*time.Millisecond)
	require.NoError(t, os.Remove(path))
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, slog.LevelDebug, levels.For("x"))
	require.Equal(t, 1, warnLines(buf), buf.String())
}

func TestWatchFileWithNoFileIsQuiet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logging.yaml")
	levels, buf := startWatch(t, path)
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, slog.LevelInfo, levels.For("x"))
	require.Empty(t, buf.String(), "a missing file is not an error")
	require.False(t, errors.Is(nil, fs.ErrNotExist))
}
