package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plexdb"
)

func pgConfig() plexdb.Config {
	return plexdb.Config{
		Host: "postgres", Port: 5432, Database: "plex", User: "plex", Password: "s3cret",
		Schema: "plex", PoolSize: 50, PoolMax: 100,
	}
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

func TestShimEnvAlwaysPassesASchema(t *testing.T) {
	// The shim builds "SET search_path TO <schema>, public" unconditionally, so
	// an absent schema is not a fallback to the default search path: it sends
	// "SET search_path TO , public", which is a syntax error. Every connection
	// then fails and Plex dies partway through its migrations, complaining
	// that its own tables do not exist.
	env := shimEnv(nil, Config{ShimLibrary: "/lib/shim.so", Postgres: pgConfig()})
	assert.Contains(t, env, "PLEX_PG_SCHEMA=plex")
}

func TestShimEnvSizesTheConnectionPool(t *testing.T) {
	// Plex opens 20 sessions to the library per process, and every pod runs
	// one, so the pool has to be sized deliberately rather than left at
	// whatever the shim defaults to.
	env := shimEnv(nil, Config{ShimLibrary: "/lib/shim.so", Postgres: pgConfig()})
	assert.Contains(t, env, "PLEX_PG_POOL_SIZE=50")
	assert.Contains(t, env, "PLEX_PG_POOL_MAX=100")
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

func TestShimEnvLeavesSIGCHLDAloneSoPlexCanStartItsPlugIns(t *testing.T) {
	// The shim sets SIGCHLD to SIG_IGN by default, to keep Plex's
	// CrashUploader from raising it on every exit. With SIGCHLD ignored the
	// kernel reaps children itself and wait() fails with ECHILD, so Plex
	// cannot manage the Python processes its plug-ins run in: the System
	// bundle never reports its port, every /:/plugins request answers 503 and
	// the server never finishes starting. It gets as far as "Media provider
	// refresh complete" and stops there, which reads like a hang rather than a
	// misconfiguration.
	//
	// We do not need what it buys. Plex runs under our own subreaper, which is
	// what absorbs the vfork re-exec, and its CrashUploader is replaced by a
	// no-op binary.
	env := shimEnv(nil, Config{ShimLibrary: "/lib/shim.so", Postgres: pgConfig()})

	assert.Contains(t, env, "PLEX_PG_DISABLE_SIGCHLD_IGNORE=1")
}

func TestShimEnvSetsNoLocaleBecausePlexCannotParseOne(t *testing.T) {
	// Plex's bundled boost::locale takes the charset from the locale name and
	// does not recognise the one in "C.utf8". It falls back to ASCII, which
	// its own build rejects, and dies while loading translations:
	//
	//	libc++abi: terminating with uncaught exception of type
	//	  boost::locale::conv::invalid_charset_error:
	//	  Invalid or unsupported charset:Invalid simple encoding ASCII
	//
	// With no locale set at all it picks its own and runs. That is what the
	// image Plex ships in does, and it is what we do: the variables were added
	// here to stop it rejecting a glibc-style en_US.UTF-8 that nothing sets
	// any more.
	env := shimEnv(nil, Config{ShimLibrary: "/lib/shim.so", Postgres: pgConfig()})

	for _, v := range env {
		name, _, _ := strings.Cut(v, "=")
		assert.NotContains(t, []string{"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE"}, name,
			"Plex picks its own locale; naming one crashes it")
	}
}

func TestTheShimLogsOnlyErrorsUnlessAskedForMore(t *testing.T) {
	// The shim defaults to INFO, which is a column type, a pool slot and a
	// statement handle for every value Plex reads. It lands on the manager's
	// own stderr, so every other component's output is buried under it, and
	// the file it keeps reaches tens of megabytes on a single start.
	env := shimEnv([]string{"PATH=/bin"}, Config{ShimLibrary: "/lib/shim.so", Postgres: pgConfig()})

	assert.Contains(t, env, "PLEX_PG_LOG_LEVEL=ERROR")
}

func TestAnOperatorCanStillTurnTheShimLogBackUp(t *testing.T) {
	// Raising it is how most of the bugs in this repo were found, so the
	// quiet default has to stay a default rather than become a decision.
	env := shimEnv(
		[]string{"PATH=/bin", "PLEX_PG_LOG_LEVEL=DEBUG"},
		Config{ShimLibrary: "/lib/shim.so", Postgres: pgConfig()},
	)

	assert.Contains(t, env, "PLEX_PG_LOG_LEVEL=DEBUG")
	assert.NotContains(t, env, "PLEX_PG_LOG_LEVEL=ERROR",
		"two values for one variable leave it to whichever libc reads it first")
}

func TestTheConnectionPoolDoesNotReclaimSlotsPlexIsStillUsing(t *testing.T) {
	// The shim's pool frees a slot once it has been idle past this timeout and
	// the thread that opened it looks dead. Neither is evidence that Plex has
	// finished with it: Plex opens twenty database handles at startup and
	// keeps them for the life of the process, while the worker threads that
	// used them come and go.
	//
	// At the default of 300 seconds that reclaimed a live connection and
	// killed Plex. The last line it ever wrote was
	//
	//	Pool PHASE 0: Freed zombie slot 4 (owner thread dead, idle 322 sec)
	//
	// Raising it past any realistic idle period stops the reclaim firing. It
	// is a workaround for an upstream bug, not a fix: the pool is bounded, so
	// the cost is only that idle connections stay open.
	env := shimEnv([]string{"PATH=/bin"}, Config{ShimLibrary: "/lib/shim.so", Postgres: pgConfig()})

	assert.Contains(t, env, "PLEX_PG_IDLE_TIMEOUT=86400")
}

func TestTheIdleTimeoutIsStillAnOperatorsToSet(t *testing.T) {
	env := shimEnv(
		[]string{"PATH=/bin", "PLEX_PG_IDLE_TIMEOUT=600"},
		Config{ShimLibrary: "/lib/shim.so", Postgres: pgConfig()},
	)

	assert.Contains(t, env, "PLEX_PG_IDLE_TIMEOUT=600")
	assert.NotContains(t, env, "PLEX_PG_IDLE_TIMEOUT=86400",
		"two values for one variable leave it to whichever libc reads it first")
}
