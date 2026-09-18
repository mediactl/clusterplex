package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plexdb"
)

func pgConfig() plexdb.Config {
	return plexdb.Config{Host: "postgres", Port: 5432, Database: "plex", User: "plex", Password: "s3cret"}
}

func TestShimEnvPreloadsTheInterposerAndPassesTheSameDatabase(t *testing.T) {
	// The shim reads its own PLEX_PG_* variables, so they have to agree with
	// what the manager connects with or the two halves read different data.
	env := shimEnv([]string{"PATH=/bin"}, Config{ShimLibrary: "/lib/shim.so", Postgres: pgConfig()})

	assert.Contains(t, env, "PATH=/bin", "the process environment is preserved")
	assert.Contains(t, env, "LD_PRELOAD=/lib/shim.so")
	assert.Contains(t, env, "PLEX_PG_HOST=postgres")
	assert.Contains(t, env, "PLEX_PG_PORT=5432")
	assert.Contains(t, env, "PLEX_PG_DATABASE=plex")
	assert.Contains(t, env, "PLEX_PG_USER=plex")
	assert.Contains(t, env, "PLEX_PG_PASSWORD=s3cret")
}

func TestShimEnvOmitsTheSchemaWhenThereIsNone(t *testing.T) {
	env := shimEnv(nil, Config{ShimLibrary: "/lib/shim.so", Postgres: pgConfig()})
	for _, e := range env {
		assert.NotContains(t, e, "PLEX_PG_SCHEMA")
	}
}

func TestShimEnvLeavesPlexAloneWhenNoShimIsConfigured(t *testing.T) {
	env := shimEnv([]string{"PATH=/bin"}, Config{Postgres: pgConfig()})
	assert.Equal(t, []string{"PATH=/bin"}, env)
}

func TestCheckShimFailsWhenTheInterposerIsMissing(t *testing.T) {
	// Plex starts happily without it and silently uses its own SQLite file, so
	// this would otherwise surface as an empty library rather than an error.
	err := checkShim(Config{ShimLibrary: filepath.Join(t.TempDir(), "absent.so")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent.so")
}

func TestCheckShimPassesWhenTheInterposerIsPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shim.so")
	require.NoError(t, os.WriteFile(path, nil, 0o644))
	require.NoError(t, checkShim(Config{ShimLibrary: path}))
}

func TestCheckShimIsSatisfiedWhenNoShimIsConfigured(t *testing.T) {
	require.NoError(t, checkShim(Config{}))
}
