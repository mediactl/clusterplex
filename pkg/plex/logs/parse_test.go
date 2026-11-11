package plexlogs

import (
	"bufio"
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/logging"
)

// records feeds every line of the fixture through one Assembler and
// returns what it completed, the open record flushed last.
func records(t *testing.T, path, subsystem string, format Format) []Record {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	a := NewAssembler(path, subsystem, format)
	var out []Record
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if r, ok := a.Line(sc.Bytes()); ok {
			out = append(out, r)
		}
	}
	if r, ok := a.Flush(); ok {
		out = append(out, r)
	}
	return out
}

func TestPMSLines(t *testing.T) {
	recs := records(t, "testdata/pms.log", "pms", FormatPMS)
	require.Len(t, recs, 7)
	first := recs[0]
	assert.Equal(t, time.Date(2026, 11, 10, 18, 20, 17, 104_000_000, time.UTC), first.Time)
	assert.Equal(t, slog.LevelInfo, first.Level)
	assert.Equal(t, "281472998296928", first.Thread)
	assert.Equal(t, "", first.Req)
	assert.Equal(t, "pms", first.Subsystem)
	assert.Equal(t, "testdata/pms.log", first.File)
	assert.True(t, strings.HasPrefix(first.Msg, "Plex Media Server v1.43.5.11079"), first.Msg)
	assert.Equal(t, slog.LevelDebug, recs[2].Level)
	e := recs[3]
	assert.Equal(t, slog.LevelError, e.Level)
	assert.Equal(t, "b5", e.Req)
	assert.Equal(t, "Error issuing curl_easy_perform(handle): 28 (Timeout was reached) (SSL handshake)", e.Msg)
	assert.Equal(t, "b5/Transcode", recs[4].Req, "a request id carries its suffix")
	assert.Equal(t, slog.LevelWarn, recs[4].Level)
	assert.Equal(t, logging.LevelTrace, recs[5].Level, "VERBOSE is TRACE")
	assert.Equal(t, "Query done in 128ms", recs[6].Msg)
}

func TestTunerLinesHaveHexThreads(t *testing.T) {
	recs := records(t, "testdata/tuner.log", "pms.tuner", FormatPMS)
	require.Len(t, recs, 1)
	assert.Equal(t, "0xffff8d7e0f90", recs[0].Thread)
	assert.Equal(t, time.Date(2026, 11, 10, 18, 21, 0, 500_000_000, time.UTC), recs[0].Time)
}

func TestPluginLines(t *testing.T) {
	recs := records(t, "testdata/plugin.log", "pms.plugin", FormatPlugin)
	require.Len(t, recs, 3)
	assert.Equal(t, time.Date(2026, 11, 10, 18, 20, 21, 343_000_000, time.UTC), recs[0].Time)
	assert.Equal(t, slog.LevelDebug, recs[0].Level)
	assert.Equal(t, "ffff893ef890", recs[0].Thread)
	assert.Contains(t, recs[0].Msg, "core:349", "the module and line stay in the message")
	assert.Contains(t, recs[0].Msg, "Starting framework core")
	assert.Equal(t, slog.LevelWarn, recs[1].Level, "WARNING is WARN")
	assert.Equal(t, slog.LevelError, recs[2].Level, "CRITICAL is ERROR")
}

func TestContinuationsFold(t *testing.T) {
	recs := records(t, "testdata/pms.log", "pms", FormatPMS)
	q := recs[5]
	assert.Equal(t, "Running a long query:\nSELECT metadata_items.id, metadata_items.title\n  FROM metadata_items\n  WHERE metadata_items.library_section_id = 2", q.Msg)
}

func TestAFirstLineWithoutATimestampIsOneInfoRecord(t *testing.T) {
	a := NewAssembler("x.log", "pms", FormatPMS)
	a.Now = func() time.Time { return time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC) }
	_, ok := a.Line([]byte("garbage before the first stamp"))
	assert.False(t, ok, "nothing completes until the next line")
	r, ok := a.Flush()
	require.True(t, ok)
	assert.Equal(t, slog.LevelInfo, r.Level)
	assert.Equal(t, "garbage before the first stamp", r.Msg)
	assert.Equal(t, time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), r.Time)
}

func TestFileFor(t *testing.T) {
	for base, want := range map[string]struct {
		subsystem, plugin string
		format            Format
		ok                bool
	}{
		"Plex Media Server.log":                    {"pms", "", FormatPMS, true},
		"Plex Media Server.3.log":                  {"pms", "", FormatPMS, true},
		"Plex Media Scanner.log":                   {"pms.scanner", "", FormatPMS, true},
		"Plex Media Scanner Deep Analysis.log":     {"pms.scanner", "", FormatPMS, true},
		"Plex Tuner Service.log":                   {"pms.tuner", "", FormatPMS, true},
		"Plex DLNA Server.log":                     {"pms.dlna", "", FormatPMS, true},
		"Plex Transcoder Statistics.log":           {"pms.transcoder", "", FormatPMS, true},
		"Plex Script Host.log":                     {"pms", "", FormatPMS, true},
		"PMS Plugin Logs/com.plexapp.system.log":   {"pms.plugin", "com.plexapp.system", FormatPlugin, true},
		"PMS Plugin Logs/com.plexapp.system.log.2": {"pms.plugin", "com.plexapp.system", FormatPlugin, true},
		"Crash Reports/1.43.5/x.dmp":               {"", "", FormatPMS, false},
		"Plex Media Server.log.lock":               {"", "", FormatPMS, false},
	} {
		subsystem, plugin, format, ok := FileFor(base)
		assert.Equal(t, want.ok, ok, base)
		assert.Equal(t, want.subsystem, subsystem, base)
		assert.Equal(t, want.plugin, plugin, base)
		if ok {
			assert.Equal(t, want.format, format, base)
		}
	}
}

func TestStem(t *testing.T) {
	for in, want := range map[string]string{
		"Plex Media Server.log":    "Plex Media Server.log",
		"Plex Media Server.3.log":  "Plex Media Server.log",
		"com.plexapp.system.log":   "com.plexapp.system.log",
		"com.plexapp.system.log.2": "com.plexapp.system.log",
		"Plex Media Server.10.log": "Plex Media Server.log",
	} {
		assert.Equal(t, want, Stem(in), in)
	}
}

func TestATokenIsRedacted(t *testing.T) {
	recs := records(t, "testdata/plugin.log", "pms.plugin", FormatPlugin)
	assert.Contains(t, recs[1].Msg, "X-Plex-Token=REDACTED&x=1")
	assert.NotContains(t, recs[1].Msg, "abc123def")
	a := NewAssembler("x.log", "pms", FormatPMS)
	a.Line([]byte("Nov 10, 2026 18:20:17.104 [1] INFO - header x-plex-token: zzz9 sent; url ?X-PLEX-TOKEN=QQQ&a=b"))
	r, ok := a.Flush()
	require.True(t, ok)
	assert.Equal(t, "header X-Plex-Token=REDACTED sent; url ?X-Plex-Token=REDACTED&a=b", r.Msg)
}

func TestAnOverlongOrBinaryLineIsOneRecord(t *testing.T) {
	a := NewAssembler("x.log", "pms", FormatPMS)
	a.Line([]byte("Nov 10, 2026 18:20:17.104 [1] INFO - trace follows"))
	long := strings.Repeat("é", 50*1024) // 100 KiB of two-byte runes
	a.Line([]byte(long))
	r, ok := a.Flush()
	require.True(t, ok)
	assert.LessOrEqual(t, len(r.Msg), MaxMsgBytes)
	assert.True(t, utf8.ValidString(r.Msg), "cut on a rune boundary")
	assert.True(t, strings.HasPrefix(r.Msg, "trace follows\né"))

	b := NewAssembler("x.log", "pms", FormatPMS)
	b.Line(append([]byte("Nov 10, 2026 18:20:17.104 [1] INFO - bytes "), 0xff, 0xfe))
	r, ok = b.Flush()
	require.True(t, ok)
	assert.Equal(t, "bytes ��", r.Msg)
}
