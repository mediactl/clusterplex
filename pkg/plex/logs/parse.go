package plexlogs

import (
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mediactl/clusterplex/pkg/logging"
)

// Record is one of Plex's log entries as the contract's record carries it.
type Record struct {
	// Time is the line's own stamp, read as UTC (the pod's clock is UTC).
	Time time.Time
	// Level is Plex's level mapped onto the contract's: VERBOSE is TRACE,
	// WARNING WARN, CRITICAL ERROR.
	Level slog.Level
	// Subsystem is the file's (FileFor), File the path it was read from,
	// Plugin the bundle id of a plugin's file.
	Subsystem, File, Plugin string
	// Thread and Req are Plex's thread id and request id, when the line
	// carries them.
	Thread, Req string
	// Msg is the message with its continuation lines joined by "\n", tokens
	// redacted, invalid UTF-8 replaced and cut at MaxMsgBytes.
	Msg string
}

// MaxMsgBytes caps a record's message (a stack trace, a dumped query).
const MaxMsgBytes = 64 << 10

var (
	pmsLine    = regexp.MustCompile(`^([A-Z][a-z]{2} \d{2}, \d{4} \d{2}:\d{2}:\d{2}\.\d{3}) \[([0-9A-Za-z]+)\] ([A-Z]+) - (?:\[Req#([^\]]*)\] )?(.*)$`)
	pluginLine = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},\d{3}) \(([0-9A-Za-z]+)\) :\s+([A-Z]+) (.*)$`)
	// token is X-Plex-Token as a query parameter or a header, any case.
	token = regexp.MustCompile(`(?i)x-plex-token(?:=|:\s*)[^&\s"']*`)
)

const (
	pmsTime    = "Jan 02, 2006 15:04:05.000"
	pluginTime = "2006-01-02 15:04:05,000"
)

// Level maps Plex's level names onto the contract's; an unknown one is INFO.
func Level(name string) slog.Level {
	switch strings.ToUpper(name) {
	case "VERBOSE":
		return logging.LevelTrace
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR", "CRITICAL":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// Assembler turns a file's lines into records: a line that starts a record
// completes the one before it, a line that does not continues it.
type Assembler struct {
	file, subsystem, plugin string
	format                  Format
	// Now stamps a first line that carries no timestamp; nil is time.Now.
	Now func() time.Time

	open    Record
	hasOpen bool
	// msg is the open record's message as it grows; full once it reached
	// MaxMsgBytes, after which continuations are dropped.
	msg  strings.Builder
	full bool
}

// NewAssembler reads file's lines, of format, as subsystem's records.
func NewAssembler(file, subsystem string, format Format) *Assembler {
	return &Assembler{file: file, subsystem: subsystem, format: format}
}

// WithPlugin sets the bundle id a plugin file's records carry.
func (a *Assembler) WithPlugin(plugin string) *Assembler {
	a.plugin = plugin
	return a
}

// Line reads one line (without its newline). It returns the record the line
// completed: the open one when this line starts a new record.
func (a *Assembler) Line(b []byte) (Record, bool) {
	line := strings.ToValidUTF8(string(b), "�")
	if rec, msg, ok := a.start(line); ok {
		done, had := a.flush()
		a.open, a.hasOpen = rec, true
		a.msg.Reset()
		a.full = false
		a.append(msg)
		return done, had
	}
	if !a.hasOpen {
		now := time.Now
		if a.Now != nil {
			now = a.Now
		}
		a.open = Record{Time: now().UTC(), Level: slog.LevelInfo, Subsystem: a.subsystem, File: a.file, Plugin: a.plugin}
		a.hasOpen = true
		a.msg.Reset()
		a.full = false
		a.append(line)
		return Record{}, false
	}
	a.append("\n" + line)
	return Record{}, false
}

// Flush returns the open record, if any, and forgets it.
func (a *Assembler) Flush() (Record, bool) { return a.flush() }

func (a *Assembler) flush() (Record, bool) {
	if !a.hasOpen {
		return Record{}, false
	}
	rec := a.open
	rec.Msg = redact(a.msg.String())
	a.hasOpen = false
	a.msg.Reset()
	return rec, true
}

// append adds s to the open message, cutting at MaxMsgBytes on a rune
// boundary.
func (a *Assembler) append(s string) {
	if a.full {
		return
	}
	room := MaxMsgBytes - a.msg.Len()
	if len(s) <= room {
		a.msg.WriteString(s)
		return
	}
	cut := room
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	a.msg.WriteString(s[:cut])
	a.full = true
}

// start parses line as the first line of a record of a's format; msg is
// its message text.
func (a *Assembler) start(line string) (rec Record, msg string, ok bool) {
	rec = Record{Subsystem: a.subsystem, File: a.file, Plugin: a.plugin}
	switch a.format {
	case FormatPlugin:
		m := pluginLine.FindStringSubmatch(line)
		if m == nil {
			return rec, "", false
		}
		t, err := time.ParseInLocation(pluginTime, m[1], time.UTC)
		if err != nil {
			return rec, "", false
		}
		rec.Time, rec.Thread, rec.Level = t, m[2], Level(m[3])
		return rec, m[4], true
	default:
		m := pmsLine.FindStringSubmatch(line)
		if m == nil {
			return rec, "", false
		}
		t, err := time.ParseInLocation(pmsTime, m[1], time.UTC)
		if err != nil {
			return rec, "", false
		}
		rec.Time, rec.Thread, rec.Level, rec.Req = t, m[2], Level(m[3]), m[4]
		return rec, m[5], true
	}
}

// redact replaces every X-Plex-Token value, as a query parameter or a
// header, so a token PMS logged in a URL never reaches the log store
// (the design's Review Focus 1).
func redact(msg string) string {
	return token.ReplaceAllString(msg, "X-Plex-Token=REDACTED")
}
