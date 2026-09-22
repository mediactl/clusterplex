package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ShimLibrary is the interposer that redirects Plex's database calls to
// PostgreSQL. It exports SQLite's own symbols, so it has to be loaded ahead of
// Plex's bundled SQLite rather than configured.
const ShimLibrary = "/usr/local/lib/plex-postgresql/db_interpose_pg.so"

// ShimLibDir holds the interposer and the libpq it links. It has to precede
// Plex's own library directory on the search path, or Plex's bundled libraries
// win and the shim resolves against the wrong ones.
const ShimLibDir = "/usr/local/lib/plex-postgresql"

// Subreaper wraps Plex so its re-exec does not look like an exit. See
// Supervisor.Subreaper.
const Subreaper = "/usr/local/bin/subreaper"

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
		"LD_LIBRARY_PATH="+ShimLibDir+":/usr/lib/plexmediaserver/lib:/usr/lib/plexmediaserver",
		// No locale is set on purpose. Plex's bundled boost::locale takes the
		// charset from the locale name, does not recognise the one in
		// "C.utf8", falls back to ASCII, and dies loading its translations:
		//
		//	boost::locale::conv::invalid_charset_error:
		//	  Invalid or unsupported charset:Invalid simple encoding ASCII
		//
		// Left to choose for itself it runs, which is what the image Plex
		// ships in does. These were set here to stop it rejecting a
		// glibc-style en_US.UTF-8 that nothing sets any more.
		//
		// The shim otherwise sets SIGCHLD to SIG_IGN, so that Plex's
		// CrashUploader cannot raise it on every exit. Ignoring SIGCHLD makes
		// the kernel reap children itself and wait() fail with ECHILD, which
		// leaves Plex unable to manage the Python processes its plug-ins run
		// in: the System bundle never reports its port and the server answers
		// 503 for ever, having logged nothing worse than "Media provider
		// refresh complete".
		//
		// We do not need it. Plex runs under our own subreaper, which is what
		// absorbs the vfork re-exec, and its CrashUploader is a no-op binary.
		"PLEX_PG_DISABLE_SIGCHLD_IGNORE=1",
	)
	// The shim defaults to INFO, which is a column type, a pool slot and a
	// statement handle for every value Plex reads: tens of megabytes on a
	// single start, on the manager's own stderr, burying everything else.
	// Raising it is how most of the bugs here were found, so this is a default
	// rather than a decision -- set PLEX_PG_LOG_LEVEL on the pod to override.
	if !hasEnv(base, "PLEX_PG_LOG_LEVEL") {
		env = append(env, "PLEX_PG_LOG_LEVEL=ERROR")
	}
	// The shim's default log file lands in Plex's state directory, which is
	// the plex-config claim every pod mounts, so three shims append to one
	// file and their lines interleave -- mid-line, on a busy start. stderr is
	// per pod, and the manager already folds Plex's stderr into its own log.
	if !hasEnv(base, "PLEX_PG_LOG_FILE") {
		env = append(env, "PLEX_PG_LOG_FILE=stderr")
	}
	return append(env, pgEnv(cfg)...)
}

// hasEnv reports whether name is already set in env.
//
// A second value for the same variable is not an override: which one a
// process sees is down to whichever its libc finds first.
func hasEnv(env []string, name string) bool {
	prefix := name + "="
	for _, v := range env {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}

// pgEnv returns only the database settings, for things that are not Plex.
//
// The preload and library path above belong to Plex alone: they put a
// musl-linked libgcc ahead of the system one, which is right for Plex and
// fatal for an ordinary glibc program. Handing them to the initialisation
// script segfaults bash before it runs a line.
func pgEnv(cfg Config) []string {
	env := []string{
		"PLEX_PG_HOST=" + cfg.Postgres.Host,
		"PLEX_PG_PORT=" + strconv.Itoa(cfg.Postgres.Port),
		"PLEX_PG_DATABASE=" + cfg.Postgres.Database,
		"PLEX_PG_USER=" + cfg.Postgres.User,
		"PLEX_PG_PASSWORD=" + cfg.Postgres.Password,
		// Not optional. The shim interpolates the schema into
		// "SET search_path TO <schema>, public" whatever it holds, so an empty
		// one is a syntax error on every connection rather than a fall back to
		// the default search path. Config validation rejects it.
		"PLEX_PG_SCHEMA=" + cfg.Postgres.Schema,
		"PLEX_PG_POOL_SIZE=" + strconv.Itoa(cfg.Postgres.PoolSize),
		"PLEX_PG_POOL_MAX=" + strconv.Itoa(cfg.Postgres.PoolMax),
	}
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
