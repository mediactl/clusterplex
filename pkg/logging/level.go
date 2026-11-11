package logging

import (
	"errors"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
)

// The two levels slog does not name. A record below DEBUG is TRACE and one
// above ERROR is FATAL (design §3); LevelName renders every level as one of
// the six names, never slog's "DEBUG-4".
const (
	LevelTrace slog.Level = -8
	LevelFatal slog.Level = 12
)

// ParseLevel reads a level as slog spells it, case-insensitively ("info",
// "WARN", "INFO+2"), plus "trace" and "fatal". "warning" is not a level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return 0, errors.New("empty log level")
	case "trace":
		return LevelTrace, nil
	case "fatal":
		return LevelFatal, nil
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.TrimSpace(s))); err != nil {
		return 0, err
	}
	return l, nil
}

// LevelName is the record's level: TRACE, DEBUG, INFO, WARN, ERROR or FATAL.
func LevelName(l slog.Level) string {
	switch {
	case l < slog.LevelDebug:
		return "TRACE"
	case l < slog.LevelInfo:
		return "DEBUG"
	case l < slog.LevelWarn:
		return "INFO"
	case l < slog.LevelError:
		return "WARN"
	case l < LevelFatal:
		return "ERROR"
	default:
		return "FATAL"
	}
}

// Levels is a process's floor and its per-subsystem overrides, read on every
// log call without a lock and replaced whole by Set (the config file
// watcher's, design §4). An override applies to its subsystem and to every
// subsystem below it by dotted segment: "livetv" covers "livetv.tuner" and
// never "livetvx".
type Levels struct {
	snap atomic.Pointer[levelSnapshot]

	mu      sync.Mutex
	nextID  int
	onChang map[int]func()
}

type levelSnapshot struct {
	floor     slog.Level
	overrides map[string]slog.Level
}

// NewLevels holds floor and overrides.
func NewLevels(floor slog.Level, overrides map[string]slog.Level) *Levels {
	l := &Levels{}
	l.snap.Store(&levelSnapshot{floor: floor, overrides: maps.Clone(overrides)})
	return l
}

// Set replaces the floor and every override, then tells each OnChange
// callback, in the caller's goroutine.
func (l *Levels) Set(floor slog.Level, overrides map[string]slog.Level) {
	l.snap.Store(&levelSnapshot{floor: floor, overrides: maps.Clone(overrides)})
	l.mu.Lock()
	fns := make([]func(), 0, len(l.onChang))
	for _, fn := range l.onChang {
		fns = append(fns, fn)
	}
	l.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// Floor is the level without an override.
func (l *Levels) Floor() slog.Level { return l.snap.Load().floor }

// Overrides is a copy of the overrides in force.
func (l *Levels) Overrides() map[string]slog.Level { return maps.Clone(l.snap.Load().overrides) }

// For is the level subsystem logs at: its most specific override by dotted
// prefix, else the floor.
func (l *Levels) For(subsystem string) slog.Level {
	s := l.snap.Load()
	for name := subsystem; name != ""; {
		if lvl, ok := s.overrides[name]; ok {
			return lvl
		}
		i := strings.LastIndexByte(name, '.')
		if i < 0 {
			break
		}
		name = name[:i]
	}
	return s.floor
}

// OnChange calls fn after every Set until cancel is called.
func (l *Levels) OnChange(fn func()) (cancel func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.onChang == nil {
		l.onChang = map[int]func(){}
	}
	id := l.nextID
	l.nextID++
	l.onChang[id] = fn
	return func() {
		l.mu.Lock()
		delete(l.onChang, id)
		l.mu.Unlock()
	}
}
