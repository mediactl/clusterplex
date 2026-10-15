package plexproject

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mediactl/clusterplex/pkg/plexseed"
)

// ErrNoItem is an apply of a file whose item is not resolved: nothing is
// written under a guessed item (spec §6 step 1).
var ErrNoItem = errors.New("plexproject: the file's item is not resolved")

// DB is what the projector needs of the pool: a transaction per file, and
// a multi-row read for the reconcile.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Projector writes one library's rows from clustarr's MediaFiles.
type Projector struct {
	DB      DB
	Seeder  *plexseed.Seeder
	Now     func() time.Time
	Refresh Refresher
	Logger  *slog.Logger
}

// Change is one MediaFile as the projection needs it.
type Change struct {
	UID      string         // the MediaFile's uid
	PlexPath string         // spec.path through the path mapper
	SeedKey  string         // clustarrwatch.SeedKey
	Item     Item           // Resolve's chain; empty when unresolved
	Input    plexseed.Input // path, size, probe hash, probe, markers, original language
	// Unresolved marks a file whose item is not in the caches yet: a
	// reconcile keeps its row and applies nothing (spec §8).
	Unresolved bool
}

// Apply is spec §6: one file, one transaction. The item chain, the folder,
// the media row, then the seeder's streams and markers, then the stamps.
// A row whose stamped seed key already is c.SeedKey is current and
// nothing is written.
func (p *Projector) Apply(ctx context.Context, section, location int64, c Change) error {
	if len(c.Item.Chain) == 0 {
		return fmt.Errorf("%w: %s", ErrNoItem, c.UID)
	}
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := p.now()

	f, have, err := findMedia(ctx, tx, section, c.UID, c.PlexPath)
	if err != nil {
		return fmt.Errorf("plexproject: find %s: %w", c.UID, err)
	}
	if have && !f.adopted && f.seedKey == c.SeedKey {
		return nil
	}
	item, err := EnsureChain(ctx, tx, section, c.Item, now, p.Refresh)
	if err != nil {
		return err
	}
	root, err := locationRoot(ctx, tx, location)
	if err != nil {
		return fmt.Errorf("plexproject: location %d: %w", location, err)
	}
	dir, err := EnsureDirectory(ctx, tx, section, root, path.Dir(c.PlexPath), now)
	if err != nil {
		return err
	}
	target, err := upsertMedia(ctx, tx, section, location, item, dir, f, have, c, now)
	if err != nil {
		return fmt.Errorf("plexproject: media row for %s: %w", c.UID, err)
	}
	if _, err := p.Seeder.SeedTx(ctx, tx, target, c.Input); err != nil {
		return fmt.Errorf("plexproject: seed %s: %w", c.UID, err)
	}
	if err := stampMedia(ctx, tx, target.Media, c); err != nil {
		return fmt.Errorf("plexproject: stamp %s: %w", c.UID, err)
	}
	return tx.Commit(ctx)
}

func (p *Projector) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func locationRoot(ctx context.Context, tx pgx.Tx, location int64) (string, error) {
	var root string
	err := tx.QueryRow(ctx, `SELECT root_path FROM section_locations WHERE id = $1`, location).Scan(&root)
	return root, err
}
