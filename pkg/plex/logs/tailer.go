package plexlogs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/mediactl/clusterplex/pkg/logging"
)

// errNoLogger is what a Tailer nobody handed a Logger answers from Run (the
// 2026-11-10 structured-logging design §5): it invents none.
var errNoLogger = errors.New("plexlogs: Logger is required")

// The Tailer's defaults (the design's §4.2, §4.4).
const (
	DefaultInterval   = time.Second
	DefaultStaleAfter = 24 * time.Hour
	DefaultBehindWarn = 10 << 20
	readChunk         = 256 << 10
	// behindSubsystem is the subsystem the falling-behind WARN carries.
	behindSubsystem = "supervisor"
)

// Tailer follows Plex's log files under Dir (<plex-dir>/Logs) and its
// plugins' under Dir/PMS Plugin Logs, writes each line as a record of its
// file's subsystem through Logger with Plex's own time and level, and
// deletes what Plex has finished with: a rotated file once read to its
// end, a live file nothing wrote to for StaleAfter (the design's §4.2,
// §4.4). Files are followed by inode, so Plex's rename at rotation keeps the
// tailer on the old file until it has read every line.
type Tailer struct {
	Dir    string
	Logger *slog.Logger
	// Interval is the scan cadence (DefaultInterval), StaleAfter how long
	// an unwritten live file stays (DefaultStaleAfter), BehindWarn the
	// unread bytes across files past which one WARN is written until the
	// backlog has once fallen below it (DefaultBehindWarn).
	Interval, StaleAfter time.Duration
	BehindWarn           int64
	Metrics              *Metrics
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu     sync.Mutex
	files  map[fileKey]*followed
	behind bool
}

// fileKey identifies a file by device and inode.
type fileKey struct{ dev, ino uint64 }

// followed is one file the tailer reads.
type followed struct {
	key       fileKey
	path      string // the name the inode carries now
	rel       string // its path below Dir, FileFor's input
	subsystem string
	plugin    string
	format    Format
	f         *os.File
	offset    int64
	partial   []byte // the bytes after the last newline, waiting for the rest
	asm       *Assembler
	seen      bool // listed in the current scan
	size      int64
	modTime   time.Time
}

func (t *Tailer) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Tailer) interval() time.Duration {
	if t.Interval > 0 {
		return t.Interval
	}
	return DefaultInterval
}

func (t *Tailer) staleAfter() time.Duration {
	if t.StaleAfter > 0 {
		return t.StaleAfter
	}
	return DefaultStaleAfter
}

func (t *Tailer) behindWarn() int64 {
	if t.BehindWarn > 0 {
		return t.BehindWarn
	}
	return DefaultBehindWarn
}

// Run scans every Interval until ctx ends, then drains once more and
// returns nil. A nil Logger is refused.
func (t *Tailer) Run(ctx context.Context) error {
	if t.Logger == nil {
		return errNoLogger
	}
	tick := time.NewTicker(t.interval())
	defer tick.Stop()
	for {
		if err := t.tick(ctx, true); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return t.Drain(context.WithoutCancel(ctx))
		case <-tick.C:
		}
	}
}

// Drain reads every file to its end and deletes every rotated file read to
// its end; the supervisor calls it at each PMS start, for what the previous
// process left. Live files are kept: Plex rotates them itself at start.
func (t *Tailer) Drain(ctx context.Context) error {
	if t.Logger == nil {
		return errNoLogger
	}
	return t.tick(ctx, false)
}

// tick is one pass: scan, warn when behind, read, delete. stale applies the
// StaleAfter rule to live files.
func (t *Tailer) tick(ctx context.Context, stale bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.files == nil {
		t.files = map[fileKey]*followed{}
	}
	if err := t.scan(ctx); err != nil {
		return err
	}
	t.warnBehind(ctx)
	now := t.now()
	for _, fl := range t.ordered() {
		key := fl.key
		if err := t.read(ctx, fl); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logging.SubsystemLogger(t.Logger, behindSubsystem).WarnContext(ctx, "Plex log: read", "file", fl.rel, "err", err)
			continue
		}
		gone := !fl.seen
		rotated := Stem(filepath.Base(fl.path)) != filepath.Base(fl.path)
		isStale := stale && now.Sub(fl.modTime) >= t.staleAfter()
		if gone || rotated || isStale {
			t.finish(ctx, fl, !gone)
			delete(t.files, key)
		}
	}
	return nil
}

// ordered is the followed files in a fixed order: the rotated ones first,
// each group by path, so a rotated file's last lines are written before
// the new live file's first and a pass reads the same way every time.
func (t *Tailer) ordered() []*followed {
	out := make([]*followed, 0, len(t.files))
	for _, fl := range t.files {
		out = append(out, fl)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := Stem(filepath.Base(out[i].path)) != filepath.Base(out[i].path), Stem(filepath.Base(out[j].path)) != filepath.Base(out[j].path)
		if ri != rj {
			return ri
		}
		return out[i].path < out[j].path
	})
	return out
}

// scan lists Dir and Dir/PluginDir, opens each new log file at its start
// and notes each known one's current name, size and modification time.
func (t *Tailer) scan(ctx context.Context) error {
	for _, fl := range t.files {
		fl.seen = false
	}
	for _, sub := range []string{"", PluginDir} {
		dir := filepath.Join(t.Dir, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			rel := filepath.ToSlash(filepath.Join(sub, e.Name()))
			subsystem, plugin, format, ok := FileFor(rel)
			if !ok {
				continue
			}
			path := filepath.Join(dir, e.Name())
			info, err := os.Stat(path)
			if err != nil {
				continue // gone between the listing and the stat
			}
			key := keyOf(info, path)
			fl, known := t.files[key]
			if !known {
				f, err := os.Open(path)
				if err != nil {
					continue
				}
				fl = &followed{
					key: key, f: f, rel: rel, subsystem: subsystem, plugin: plugin, format: format,
					asm: NewAssembler(filepath.Base(Stem(e.Name())), subsystem, format).WithPlugin(plugin),
				}
				fl.asm.Now = t.now
				t.files[key] = fl
			}
			fl.seen, fl.path, fl.size, fl.modTime = true, path, info.Size(), info.ModTime()
		}
	}
	return nil
}

// keyOf is the file's device and inode, or a key from its path where the
// platform gives none.
func keyOf(info fs.FileInfo, path string) fileKey {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return fileKey{dev: uint64(st.Dev), ino: st.Ino} //nolint:unconvert // Dev's type differs by platform
	}
	h := uint64(14695981039346656037)
	for i := 0; i < len(path); i++ {
		h ^= uint64(path[i])
		h *= 1099511628211
	}
	return fileKey{ino: h}
}

// warnBehind writes one WARN when the unread bytes across the followed
// files pass BehindWarn, and arms it again once they fell below.
func (t *Tailer) warnBehind(ctx context.Context) {
	var backlog int64
	for _, fl := range t.files {
		if fl.seen && fl.size > fl.offset {
			backlog += fl.size - fl.offset
		}
	}
	switch {
	case backlog > t.behindWarn() && !t.behind:
		t.behind = true
		logging.SubsystemLogger(t.Logger, behindSubsystem).WarnContext(ctx,
			"Plex writes its logs faster than they are read; falling behind", "bytes", backlog, "limit", t.behindWarn())
	case backlog <= t.behindWarn():
		t.behind = false
	}
}

// read takes fl from its offset to its end, each complete line through the
// assembler; a file shorter than the offset was truncated and is read from
// its start.
func (t *Tailer) read(ctx context.Context, fl *followed) error {
	info, err := fl.f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < fl.offset {
		t.flushOpen(ctx, fl)
		fl.partial = nil
		fl.offset = 0
	}
	if _, err := fl.f.Seek(fl.offset, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, readChunk)
	for {
		n, err := fl.f.Read(buf)
		if n > 0 {
			fl.offset += int64(n)
			if t.Metrics != nil {
				t.Metrics.bytes.WithLabelValues(Stem(filepath.Base(fl.rel))).Add(float64(n))
			}
			data := make([]byte, 0, len(fl.partial)+n)
			data = append(data, fl.partial...)
			data = append(data, buf[:n]...)
			for {
				i := bytes.IndexByte(data, '\n')
				if i < 0 {
					break
				}
				t.line(ctx, fl, data[:i])
				data = data[i+1:]
			}
			fl.partial = append([]byte(nil), data...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}
	// The record a complete line opened is written now rather than held
	// for a continuation that may never come: a live file's last line
	// would otherwise wait on Plex's next one.
	t.flushOpen(ctx, fl)
	return nil
}

// line feeds one complete line to fl's assembler and writes what it
// completed.
func (t *Tailer) line(ctx context.Context, fl *followed, b []byte) {
	if rec, ok := fl.asm.Line(b); ok {
		t.write(ctx, rec)
	}
}

// flushOpen writes the record fl's assembler holds open.
func (t *Tailer) flushOpen(ctx context.Context, fl *followed) {
	if rec, ok := fl.asm.Flush(); ok {
		t.write(ctx, rec)
	}
}

// finish writes fl's partial last line and open record, closes it and, when
// remove is set, deletes the file by the name its inode carries now, unless
// that name belongs to another inode by now.
func (t *Tailer) finish(ctx context.Context, fl *followed, remove bool) {
	if len(fl.partial) > 0 {
		t.line(ctx, fl, fl.partial)
		fl.partial = nil
	}
	t.flushOpen(ctx, fl)
	_ = fl.f.Close()
	if !remove {
		return
	}
	info, err := os.Stat(fl.path)
	if err != nil || keyOf(info, fl.path) != fl.key {
		return // Plex removed or replaced it already
	}
	if err := os.Remove(fl.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logging.SubsystemLogger(t.Logger, behindSubsystem).WarnContext(ctx, "Plex log: delete", "file", fl.rel, "err", err)
		return
	}
	stem := Stem(filepath.Base(fl.rel))
	if t.Metrics != nil {
		t.Metrics.deleted.WithLabelValues(stem).Inc()
	}
	logging.SubsystemLogger(t.Logger, behindSubsystem).DebugContext(ctx, "Plex log: deleted", "file", filepath.Base(fl.path), "live", stem)
}

// write hands rec to the logger as the record Plex wrote: its own time and
// level under its file's subsystem, counted whether or not the level is
// enabled.
func (t *Tailer) write(ctx context.Context, rec Record) {
	if t.Metrics != nil {
		t.Metrics.lines.WithLabelValues(rec.Subsystem, logging.LevelName(rec.Level)).Inc()
	}
	h := logging.SubsystemLogger(t.Logger, rec.Subsystem).Handler()
	if !h.Enabled(ctx, rec.Level) {
		return
	}
	r := slog.NewRecord(rec.Time, rec.Level, rec.Msg, 0)
	if rec.Thread != "" {
		r.AddAttrs(slog.String("thread", rec.Thread))
	}
	if rec.Req != "" {
		r.AddAttrs(slog.String("req", rec.Req))
	}
	if rec.File != "" {
		r.AddAttrs(slog.String("file", rec.File))
	}
	if rec.Plugin != "" {
		r.AddAttrs(slog.String("plugin", rec.Plugin))
	}
	_ = h.Handle(ctx, r)
}
