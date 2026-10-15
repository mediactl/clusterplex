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
	// Location is the section_locations id the file falls under, when the
	// section has several; zero means the one the caller passed.
	Location int64
}

// Apply is spec §6: one file, one transaction. The item chain, the folder,
// the media row, then the seeder's streams and markers, then the stamps.
// A row whose stamped seed key, item key and seed version already match is
// current and nothing is written. Refreshes are asked for only once the
// transaction committed, so a rolled-back insert asks for none.
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
	if have && !f.adopted && f.seedKey == c.SeedKey && f.itemKey == c.Item.Key() && f.seedVersion == SeedVersion {
		return nil
	}
	pending := &pendingRefresh{}
	item, err := EnsureChain(ctx, tx, section, c.Item, now, pending)
	if err != nil {
		return err
	}
	if c.Location != 0 {
		location = c.Location
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
	if have && f.item != 0 && f.item != item {
		// The file moved to another item: the one it left is recounted and
		// trashed if nothing else is on it (review I7).
		if err := trashIfEmpty(ctx, tx, f.item, now.Unix()); err != nil {
			return fmt.Errorf("plexproject: recount item %d: %w", f.item, err)
		}
	}
	// A new row, an adopted one, a changed probe or an older row format:
	// the row's streams are the probe's and nothing else. Otherwise (a path
	// change alone) the seeder keeps them and fixes languages.
	seeded := c.Input.Probe != nil && c.Input.Probe.RuntimeMillis > 0
	replace := !have || f.adopted || f.probeHash != c.Input.ProbeHash || f.seedVersion != SeedVersion
	if replace {
		_, err = p.Seeder.ReplaceTx(ctx, tx, target, c.Input)
	} else {
		_, err = p.Seeder.SeedTx(ctx, tx, target, c.Input)
	}
	if err != nil {
		return fmt.Errorf("plexproject: seed %s: %w", c.UID, err)
	}
	if err := stampMedia(ctx, tx, target.Media, c, seeded); err != nil {
		return fmt.Errorf("plexproject: stamp %s: %w", c.UID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	pending.deliver(p.Refresh)
	return nil
}

// pendingRefresh holds the ids EnsureChain inserted until the transaction
// commits (review I11).
type pendingRefresh struct{ ids []int64 }

func (r *pendingRefresh) NeedsRefresh(id int64) { r.ids = append(r.ids, id) }

func (r *pendingRefresh) deliver(to Refresher) {
	if to == nil {
		return
	}
	for _, id := range r.ids {
		to.NeedsRefresh(id)
	}
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
