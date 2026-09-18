// Package plexdb reads the few facts about Plex's library that the proxy needs.
//
// Plex's library lives in PostgreSQL, reached through the plex-postgresql shim
// that translates Plex's SQLite calls. We read the same database directly with
// ordinary SQL rather than going through that shim: a read of one column does
// not need translating, and going direct means any pod can resolve a media
// part without Plex or its database file being present locally.
package plexdb

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound means the library has no such part.
var ErrNotFound = errors.New("part not found")

// ErrNoRows is what a Querier returns when a query matched nothing. It exists
// so callers need not import a driver package to recognise the case.
var ErrNoRows = errors.New("no rows in result set")

// cacheTTL bounds how long a resolved path is reused. Seeking produces a burst
// of range requests for one part, and a file's path rarely changes.
const cacheTTL = 5 * time.Minute

// Row is one result row.
type Row interface {
	Scan(dest ...any) error
}

// Querier runs a query returning at most one row. A pgx pool satisfies it
// through Pool below.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) Row
}

// Config is how to reach the library database. The field names deliberately
// mirror the shim's own PLEX_PG_* settings so that both halves of the system
// are configured from one place and cannot drift apart.
type Config struct {
	Host     string
	Port     int
	Database string
	User     string
	Password string
	// Schema is the PostgreSQL schema holding Plex's tables. It is required,
	// not a convenience: the shim interpolates it into
	// "SET search_path TO <schema>, public" whatever it holds, so leaving it
	// empty is a syntax error on every connection rather than a fall back to
	// the default search path.
	Schema string
	// PoolSize and PoolMax size the shim's connection pool. Plex opens 20
	// sessions to the library per process and every pod runs one, so the
	// database's own max_connections has to cover PoolMax times the number of
	// pods.
	PoolSize int
	PoolMax  int
	// SSLMode is libpq's sslmode; "disable" for an in-cluster database.
	SSLMode string
}

// Validate reports whether the settings are complete enough to connect.
func (c Config) Validate() error {
	var missing []string
	if c.Host == "" {
		missing = append(missing, "host")
	}
	if c.Database == "" {
		missing = append(missing, "database")
	}
	if c.User == "" {
		missing = append(missing, "user")
	}
	if c.Schema == "" {
		missing = append(missing, "schema")
	}
	if len(missing) > 0 {
		return fmt.Errorf("postgres %s must be set", strings.Join(missing, ", "))
	}
	return nil
}

// DSN renders a libpq connection string.
func (c Config) DSN() string {
	sslMode := c.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}
	parts := []string{
		"host=" + c.Host,
		"port=" + strconv.Itoa(c.Port),
		"dbname=" + c.Database,
		"user=" + c.User,
		"sslmode=" + sslMode,
	}
	if c.Password != "" {
		parts = append(parts, "password="+c.Password)
	}
	if c.Schema != "" {
		parts = append(parts, "search_path="+c.Schema)
	}
	return strings.Join(parts, " ")
}

// String describes the connection without its password, so the settings can be
// logged at startup.
func (c Config) String() string {
	return fmt.Sprintf("postgres://%s@%s:%d/%s", c.User, c.Host, c.Port, c.Database)
}

// DB is a read-only view of Plex's library.
type DB struct {
	Querier Querier

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	file string
	at   time.Time
}

// partFileQuery reads the path backing a media part. Plex marks a removed part
// with deleted_at rather than deleting the row.
const partFileQuery = `SELECT file FROM media_parts WHERE id = $1 AND deleted_at IS NULL`

// PartFile returns the file backing a media part.
func (d *DB) PartFile(ctx context.Context, partID string) (string, error) {
	id, err := strconv.ParseInt(partID, 10, 64)
	if err != nil || id <= 0 {
		return "", fmt.Errorf("invalid part id %q", partID)
	}
	if file, ok := d.lookup(partID); ok {
		return file, nil
	}

	var file string
	switch err := d.Querier.QueryRow(ctx, partFileQuery, id).Scan(&file); {
	case errors.Is(err, ErrNoRows):
		return "", fmt.Errorf("part %s: %w", partID, ErrNotFound)
	case err != nil:
		return "", fmt.Errorf("query part %s: %w", partID, err)
	case file == "":
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
