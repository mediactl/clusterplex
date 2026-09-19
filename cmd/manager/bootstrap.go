package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// InitScript is upstream's own initialisation, shipped in the image.
//
// It prepares the PostgreSQL schema, rebuilds the SQLite shadow databases the
// shim keeps beside it, and creates the directories Plex expects. We run it
// rather than reimplementing it: the shim is particular about the state it
// starts from, and every difference we introduced turned into a crash that
// looked like something else.
//
// It is written as an s6 init script, so it sets up and exits without starting
// Plex, which is the half we want.
const InitScript = "/usr/local/lib/plex-postgresql/standalone-entrypoint.sh"

// prepareDatabases runs that script and waits for it.
//
// Its output goes to the log as the script writes it, because when this fails
// it is the only account of what happened — the manager deliberately knows
// nothing about the schema.
func (m *Manager) prepareDatabases(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "bash", m.Config.InitScript)
	// The script reads PLEX_PG_* from the environment, the same ones the shim
	// reads, so it is given exactly what Plex will be given.
	cmd.Env = append(os.Environ(), pgEnv(m.Config)...)
	out, err := cmd.CombinedOutput()
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			m.Logger.Info(line, "component", "plex-postgresql-init")
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", m.Config.InitScript, err)
	}
	return nil
}
