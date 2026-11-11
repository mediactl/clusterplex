package logging

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var goldenService = Service{Namespace: "cluster-plex", Name: "manager", Version: "1.2.3", InstanceID: "manager-0"}

// The contract's record (design §3): one JSON object per line, its keys in
// the order the design fixes -- time (UTC), level (named), msg, the service
// group, the subsystem, the logger's own attributes in order, then the
// record's -- with the one error key err. The golden holds a line at each
// of the six levels.
func TestRecordMatchesTheGolden(t *testing.T) {
	var buf bytes.Buffer
	h := NewHandler(&buf, "json", false, goldenService, NewLevels(LevelTrace, nil))
	sub := h.WithSubsystem("livetv.tuner").WithAttrs([]slog.Attr{
		slog.String("trace_id", "4bf92f3577b34da6a3ce929d0e0e4736"),
		slog.String("span_id", "00f067aa0ba902b7"),
		slog.String("namespace", "clustarr-system"),
		slog.String("name", "argon-tv"),
		slog.String("kind", "IPTVProvider"),
	})
	at := time.Date(2026, 11, 10, 18, 20, 17, 104000000, time.FixedZone("CST", -6*3600))
	for _, lvl := range []slog.Level{LevelTrace, slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError, LevelFatal} {
		rec := slog.NewRecord(at, lvl, "tuner started", 0)
		rec.AddAttrs(slog.Any("err", errors.New("boom")), slog.Int("ratingKey", 42))
		require.NoError(t, sub.Handle(context.Background(), rec))
	}
	want, err := os.ReadFile("testdata/record.golden.jsonl")
	require.NoError(t, err)
	require.Equal(t, string(want), buf.String())
}

func TestLevelNames(t *testing.T) {
	for lvl, want := range map[slog.Level]string{
		-9: "TRACE", -8: "TRACE", -5: "TRACE", -4: "DEBUG", -1: "DEBUG", 0: "INFO",
		4: "WARN", 8: "ERROR", 11: "ERROR", 12: "FATAL", 16: "FATAL",
	} {
		require.Equal(t, want, LevelName(lvl), "level %d", lvl)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"trace": LevelTrace, "TRACE": LevelTrace, "debug": slog.LevelDebug, "Info": slog.LevelInfo,
		"WARN": slog.LevelWarn, "error": slog.LevelError, "fatal": LevelFatal, "INFO+2": 2,
	} {
		got, err := ParseLevel(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, bad := range []string{"warning", "", "loud"} {
		_, err := ParseLevel(bad)
		require.Error(t, err, "%q", bad)
	}
}

// An override covers its subsystem and every one below it by dotted
// segment, never a mere string prefix (design §4).
func TestOverridesMatchByDottedSegment(t *testing.T) {
	l := NewLevels(slog.LevelInfo, map[string]slog.Level{"livetv": slog.LevelWarn, "livetv.tuner": slog.LevelDebug})
	for name, want := range map[string]slog.Level{
		"livetv.tuner": slog.LevelDebug, "livetv.tuner.relay": slog.LevelDebug, "livetv.relay": slog.LevelWarn,
		"livetv": slog.LevelWarn, "livetvx": slog.LevelInfo, "": slog.LevelInfo,
	} {
		require.Equal(t, want, l.For(name), "%q", name)
	}
}

func TestSubsystemIsOneKeyAndTheInnerWins(t *testing.T) {
	var buf bytes.Buffer
	root := New(Options{Level: slog.LevelInfo, Output: &buf, Subsystems: map[string]slog.Level{"remediation.transcode": slog.LevelError}})
	ctx := Subsystem(Subsystem(NewContext(context.Background(), root), "remediation"), "remediation.transcode")
	FromContext(ctx).Warn("planned")
	require.Empty(t, buf.String(), "the inner subsystem's level gates the line")
	FromContext(ctx).Error("failed")
	line := buf.String()
	require.Equal(t, 1, strings.Count(line, `"subsystem"`), line)
	require.Contains(t, line, `"subsystem":"remediation.transcode"`)
}

func TestWithKeepsTheSubsystem(t *testing.T) {
	var buf bytes.Buffer
	root := New(Options{Level: slog.LevelInfo, Output: &buf, Subsystems: map[string]slog.Level{"livetv": slog.LevelWarn}})
	ctx := With(Subsystem(NewContext(context.Background(), root), "livetv.tuner"), "trace_id", "x")
	FromContext(ctx).Debug("dropped")
	FromContext(ctx).Info("dropped too: the override is warn")
	require.Empty(t, buf.String())
	FromContext(ctx).Warn("kept")
	line := buf.String()
	require.Less(t, strings.Index(line, `"subsystem":"livetv.tuner"`), strings.Index(line, `"trace_id":"x"`), line)
}

func TestGroupsReplay(t *testing.T) {
	var buf bytes.Buffer
	root := New(Options{Output: &buf})
	SubsystemLogger(root, "x").WithGroup("g").With("a", 1).Info("hi", "b", 2)
	require.Contains(t, buf.String(), `"subsystem":"x","g":{"a":1,"b":2}`)
}

func TestTextFormatCarriesTheSameAttributes(t *testing.T) {
	var buf bytes.Buffer
	root := New(Options{Output: &buf, Format: "text", Service: goldenService})
	SubsystemLogger(root, "ui").Info("served")
	for _, want := range []string{"level=INFO", "service.name=manager", "service.namespace=cluster-plex", "subsystem=ui", `msg=served`} {
		require.Contains(t, buf.String(), want)
	}
}

func TestWatchSeesAnOverride(t *testing.T) {
	var buf bytes.Buffer
	root := New(Options{Output: &buf})
	ctx := NewContext(context.Background(), root)
	var seen []bool
	cancel := Watch(ctx, "ffmpeg", slog.LevelDebug, func(on bool) { seen = append(seen, on) })
	require.Equal(t, []bool{false}, seen)
	lv := LevelsOf(root)
	require.NotNil(t, lv)
	lv.Set(slog.LevelInfo, map[string]slog.Level{"ffmpeg": slog.LevelDebug})
	require.Equal(t, []bool{false, true}, seen)
	lv.Set(slog.LevelInfo, map[string]slog.Level{"ffmpeg": slog.LevelDebug}) // no change, no call
	require.Equal(t, []bool{false, true}, seen)
	lv.Set(slog.LevelInfo, nil)
	require.Equal(t, []bool{false, true, false}, seen)
	cancel()
	lv.Set(slog.LevelInfo, map[string]slog.Level{"ffmpeg": LevelTrace})
	require.Equal(t, []bool{false, true, false}, seen)
}

func TestLevelsOfAForeignLoggerIsNil(t *testing.T) {
	var buf bytes.Buffer
	foreign := slog.New(slog.NewJSONHandler(&buf, nil))
	require.Nil(t, LevelsOf(foreign))
	FromContext(Subsystem(NewContext(context.Background(), foreign), "x")).Info("hi")
	require.Contains(t, buf.String(), `"subsystem":"x"`)
	calls := 0
	Watch(NewContext(context.Background(), foreign), "x", slog.LevelDebug, func(bool) { calls++ })()
	require.Equal(t, 1, calls, "a foreign logger is watched as never enabled, once")
}
