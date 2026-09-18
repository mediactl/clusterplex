package main

import (
	"fmt"
	"os"
	"strconv"
)

// ShimLibrary is the interposer that redirects Plex's database calls to
// PostgreSQL. It exports SQLite's own symbols, so it has to be loaded ahead of
// Plex's bundled SQLite rather than configured.
const ShimLibrary = "/usr/local/lib/plex-postgresql/db_interpose_pg.so"

// shimEnv returns the environment Plex is started with: this process's own,
// plus the preload and the database settings the shim reads.
//
// The shim takes its connection from PLEX_PG_* variables rather than from
// anything we pass it, so these have to match what the manager itself connects
// with. Both come from one config to keep them from drifting apart.
func shimEnv(base []string, cfg Config) []string {
	if cfg.ShimLibrary == "" {
		return base
	}
	env := append([]string(nil), base...)
	env = append(env,
		"LD_PRELOAD="+cfg.ShimLibrary,
		"PLEX_PG_HOST="+cfg.Postgres.Host,
		"PLEX_PG_PORT="+strconv.Itoa(cfg.Postgres.Port),
		"PLEX_PG_DATABASE="+cfg.Postgres.Database,
		"PLEX_PG_USER="+cfg.Postgres.User,
		"PLEX_PG_PASSWORD="+cfg.Postgres.Password,
		// Not optional. The shim interpolates the schema into
		// "SET search_path TO <schema>, public" whatever it holds, so an empty
		// one is a syntax error on every connection rather than a fall back to
		// the default search path. Config validation rejects it.
		"PLEX_PG_SCHEMA="+cfg.Postgres.Schema,
		"PLEX_PG_POOL_SIZE="+strconv.Itoa(cfg.Postgres.PoolSize),
		"PLEX_PG_POOL_MAX="+strconv.Itoa(cfg.Postgres.PoolMax),
	)
	return env
}

// checkShim reports whether the interposer is actually present. Plex starts
// perfectly well without it and quietly uses its own SQLite file instead, so a
// missing library would show up as an empty library rather than as an error.
func checkShim(cfg Config) error {
	if cfg.ShimLibrary == "" {
		return nil
	}
	if _, err := os.Stat(cfg.ShimLibrary); err != nil {
		return fmt.Errorf("PostgreSQL shim %s: %w", cfg.ShimLibrary, err)
	}
	return nil
}
