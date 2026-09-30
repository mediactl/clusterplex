package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	plexbootstrap "github.com/mediactl/clusterplex/pkg/plex/bootstrap"
)

// SchemaDir is where the image puts the shim's SQL files.
const SchemaDir = "/usr/local/lib/plex-postgresql"

// plexTempDir is created world-writable for Plex, as upstream's image does.
const plexTempDir = "/run/plex-temp"

// prepareDatabases prepares the PostgreSQL schema, rebuilds the SQLite shadow
// databases the shim keeps beside it, and creates the directories Plex
// expects, before Plex starts.
//
// This was upstream's own initialisation script, run with bash. It is Go now
// (pkg/plex/bootstrap), which is what lets the image carry no shell, psql,
// sqlite3 or Python. The port follows the script step for step, because the
// shim is particular about the state it starts from.
func (m *Manager) prepareDatabases(ctx context.Context) error {
	cfg, err := pgx.ParseConfig(m.Config.Postgres.DSN())
	if err != nil {
		return fmt.Errorf("postgres settings: %w", err)
	}
	return plexbootstrap.Run(ctx, m.Logger.With("component", "plex-postgresql-init"), plexbootstrap.Options{
		Connect:      plexbootstrap.ConnectWith(cfg),
		Schema:       m.Config.Postgres.Schema,
		SchemaDir:    m.Config.SchemaDir,
		PlexDir:      m.Config.PlexDir,
		TempDir:      plexTempDir,
		ShadowTables: plexbootstrap.ShadowTables(m.Config.ShadowSyncTables),
	})
}
