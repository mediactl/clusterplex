// Package plexboot prepares the databases Plex needs before it starts.
//
// Two things have to be true before Plex runs against PostgreSQL. The shared
// database must already hold Plex's schema, because the interposing shim
// expects a database Plex has already migrated rather than an empty one — Plex
// dies part way through its own migrations otherwise, insisting its tables do
// not exist. And each pod needs a local SQLite "shadow" carrying the same
// schema, which the shim keeps alongside PostgreSQL for the things it cannot
// translate.
//
// The shadow is rebuilt on every start rather than kept. The shim issues DDL
// against it as it runs, so a shadow that survives restarts drifts away from
// what PostgreSQL holds, and the drift shows up as Plex re-running migrations
// it has already applied.
package plexboot

import (
	"fmt"
	"strings"
)

// shadowDatabases are the SQLite files Plex opens beside the library. Both get
// the same schema; the blobs one is not optional, it is simply not touched
// until Plex asks for it.
var shadowDatabases = []string{
	"com.plexapp.plugins.library.db",
	"com.plexapp.plugins.library.blobs.db",
}

// ShadowDatabases returns the SQLite files to rebuild.
func ShadowDatabases() []string {
	return append([]string(nil), shadowDatabases...)
}

// MigrationInsert renders the statements that copy PostgreSQL's applied
// migrations into a shadow database.
//
// Plex decides what to migrate from the shadow, so a shadow that lists fewer
// migrations than PostgreSQL holds sends Plex back through hundreds it has
// already applied — against a database where they have been applied — while
// the rest of startup is running.
func MigrationInsert(versions []string) string {
	var b strings.Builder
	for _, v := range versions {
		// Quoted, always. Most versions look like numbers but the shim writes
		// its own marker, and one unquoted value fails the whole batch.
		fmt.Fprintf(&b, "INSERT OR IGNORE INTO schema_migrations (version) VALUES ('%s');\n",
			strings.ReplaceAll(v, "'", "''"))
	}
	return b.String()
}
