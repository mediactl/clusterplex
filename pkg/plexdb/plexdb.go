// Package plexdb reads the few facts about Plex's library that the proxy needs.
//
// Queries go through Plex's own bundled SQLite binary rather than a Go driver:
// it is the exact build Plex uses, so it agrees with Plex about locking and
// about the custom page handling the database relies on, and it needs no cgo.
package plexdb

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrNotFound means the library has no such part.
var ErrNotFound = errors.New("part not found")

// DefaultSQLite is where Plex's SQLite binary lives in the image.
const DefaultSQLite = "/usr/lib/plexmediaserver/Plex SQLite"

// cacheTTL bounds how long a resolved path is reused. Seeking produces a burst
// of range requests for the same part, and a file's path rarely changes.
const cacheTTL = 5 * time.Minute

// Runner executes the SQLite binary. It exists so tests need no database.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner runs the binary with os/exec.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

// DB is a read-only view of Plex's library database.
type DB struct {
	Runner Runner
	// SQLite is the sqlite binary; DefaultSQLite when empty.
	SQLite string
	// Path is the library database file.
	Path string

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	file string
	at   time.Time
}

// PartFile returns the file backing a media part. The identifier must be
// digits only, which is the only shape Plex generates, so nothing else can
// reach the query.
func (d *DB) PartFile(ctx context.Context, partID string) (string, error) {
	if partID == "" || strings.TrimLeft(partID, "0123456789") != "" {
		return "", fmt.Errorf("invalid part id %q", partID)
	}
	if file, ok := d.lookup(partID); ok {
		return file, nil
	}

	binary := d.SQLite
	if binary == "" {
		binary = DefaultSQLite
	}
	out, err := d.Runner.Run(ctx, binary, d.Path, fmt.Sprintf("select file from media_parts where id=%s and deleted_at is null;", partID))
	if err != nil {
		return "", fmt.Errorf("query part %s: %w", partID, err)
	}
	file := strings.TrimSpace(out)
	if file == "" {
		return "", fmt.Errorf("part %s: %w", partID, ErrNotFound)
	}

	d.store(partID, file)
	return file, nil
}

func (d *DB) lookup(partID string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.cache[partID]
	if !ok || time.Since(entry.at) > cacheTTL {
		return "", false
	}
	return entry.file, true
}

func (d *DB) store(partID, file string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cache == nil {
		d.cache = map[string]cached{}
	}
	d.cache[partID] = cached{file: file, at: time.Now()}
}
