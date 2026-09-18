package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mediactl/clusterplex/pkg/plexboot"
)

// SchemaDir holds the SQL the image ships: the library schema loaded into
// PostgreSQL once, and the SQLite schema each pod's shadow is rebuilt from.
const SchemaDir = "/usr/local/lib/plex-postgresql"

// PlexSQLite is Plex's own SQLite build. The shadow databases have to be made
// with it rather than a system sqlite3: they carry Plex's collations and
// virtual tables, and a stock build cannot create them.
const PlexSQLite = "/usr/lib/plexmediaserver/Plex SQLite"

// shadowDir is where Plex keeps the databases the shim shadows.
func (c Config) shadowDir() string {
	return filepath.Join(c.PlexDir, "Plug-in Support", "Databases")
}

// buildShadow replaces one shadow database with a fresh one built from script.
//
// The old file is removed rather than updated, along with its write-ahead log:
// leaving those behind gives Plex a database whose journal disagrees with it.
// Errors from the script itself are reported but not fatal — the schema
// contains virtual tables that only Plex Media Server can create, and Plex
// creates them on first use.
func (m *Manager) buildShadow(ctx context.Context, path, script string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", filepath.Base(path)+suffix, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	cmd := exec.CommandContext(ctx, m.Config.SQLiteBinary, path)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil || len(out) > 0 {
		// Expected, and not fatal. The schema declares virtual tables — fts4
		// with Plex's collating tokenizer, spellfix1 — that only Plex Media
		// Server itself can construct, so building it standalone always
		// reports those and exits non-zero. Plex creates them on first use.
		m.Logger.Debug("shadow build reported errors",
			"database", filepath.Base(path), "error", err, "output", string(out))
	}
	// What matters is whether the result is usable, so that is checked rather
	// than inferred from the exit status.
	return m.checkShadow(ctx, path)
}

// checkShadow confirms a rebuilt shadow database can actually be read.
func (m *Manager) checkShadow(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, m.Config.SQLiteBinary, path,
		"select count(*) from schema_migrations")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s is not usable: %w: %s", filepath.Base(path), err, out)
	}
	return nil
}

// prepareDatabases makes PostgreSQL and this pod's shadows ready for Plex.
func (m *Manager) prepareDatabases(ctx context.Context) error {
	boot := &plexboot.Bootstrap{
		DB:          m.pool,
		Schema:      m.Config.Postgres.Schema,
		SQL:         os.DirFS(m.Config.SchemaDir),
		ShadowDir:   m.Config.shadowDir(),
		BuildShadow: m.buildShadow,
		Logger:      m.Logger.With("component", "bootstrap"),
	}
	return boot.Prepare(ctx)
}
