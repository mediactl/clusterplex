package plexboot

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrationSyncQuotesEveryVersion(t *testing.T) {
	// Versions are text. Most look like numbers, but the shim writes its own
	// marker, and an unquoted pg_adapter_1.0.0 is a syntax error that stops
	// the rest of the batch.
	sql := MigrationInsert([]string{"202210260100", "pg_adapter_1.0.0"})

	assert.Contains(t, sql, "INSERT OR IGNORE INTO schema_migrations (version) VALUES ('202210260100');")
	assert.Contains(t, sql, "('pg_adapter_1.0.0');")
}

func TestMigrationSyncEscapesQuotes(t *testing.T) {
	sql := MigrationInsert([]string{"it's"})
	assert.Contains(t, sql, "('it''s')")
}

func TestNoVersionsProducesNoStatements(t *testing.T) {
	assert.Empty(t, strings.TrimSpace(MigrationInsert(nil)))
}

func TestBothShadowDatabasesAreRebuilt(t *testing.T) {
	// Plex opens a blobs database alongside the library one, and it needs the
	// same schema. Missing it is not an error until Plex asks for it.
	names := ShadowDatabases()
	require.Len(t, names, 2)
	assert.Contains(t, names, "com.plexapp.plugins.library.db")
	assert.Contains(t, names, "com.plexapp.plugins.library.blobs.db")
}
