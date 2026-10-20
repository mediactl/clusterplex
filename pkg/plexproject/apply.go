package plexproject

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
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
	// Files is the disk the sweep asks whether a file is gone; nil
	// sweeps nothing.
	Files Files
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

	f, have, err := findMedia(ctx, tx, section, c.UID, c.PlexPath, c.Item.guids())
	if err != nil {
		return fmt.Errorf("plexproject: find %s: %w", c.UID, err)
	}
	if have && !f.adopted && c.current(f.seedKey, f.itemKey, f.seedVersion) {
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
	// change alone) the seeder keeps them and fixes languages. Without a
	// probe there is nothing to replace them with, so whatever the row
	// carries -- Plex's own analysis included -- stays until one arrives.
	seeded := c.seedable()
	replace := seeded && (!have || f.adopted || f.probeHash != c.Input.ProbeHash || f.seedVersion != SeedVersion)
	if replace {
		_, err = p.Seeder.ReplaceTx(ctx, tx, target, c.Input)
	} else {
		_, err = p.Seeder.SeedTx(ctx, tx, target, c.Input)
	}
	if err != nil {
		return fmt.Errorf("plexproject: seed %s: %w", c.UID, err)
	}
	if seeded {
		// The streams are the projection's: the subtitle files beside the
		// file are written as Plex's scanner would have found them.
		if _, _, err := p.Seeder.SyncSidecarsTx(ctx, tx, target, c.PlexPath, c.Input.Sidecars); err != nil {
			return fmt.Errorf("plexproject: sidecars of %s: %w", c.UID, err)
		}
	}
	if err := stampMedia(ctx, tx, target.Media, c, seeded); err != nil {
		return fmt.Errorf("plexproject: stamp %s: %w", c.UID, err)
	}
	if f.fileChanged(have, c, seeded) {
		// The item Plex already shows has a new file. Plex's refresh of an
		// unchanged provider answer writes only refreshed_at, so the item's
		// updated_at -- clients' updatedAt -- is stamped here, and the
		// refresh after the commit sends open clients Plex's timeline
		// "updated" for it (talos, 2026-10-20).
		if _, err := tx.Exec(ctx, `UPDATE metadata_items SET updated_at = $2 WHERE id = $1`, item, now.Unix()); err != nil {
			return fmt.Errorf("plexproject: stamp item %d: %w", item, err)
		}
		pending.NeedsRefresh(item)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	pending.deliver(p.Refresh)
	return nil
}

// seedable reports a change whose probe the seeder can write: one with a
// runtime, as plexseed.Seeder.seedTarget requires.
func (c Change) seedable() bool {
	return c.Input.Probe != nil && c.Input.Probe.RuntimeMillis > 0
}

// current reports whether a row stamped seedKey, itemKey and seedVersion
// already is c. The seed version is the stamp of seeded streams, so it is
// asked of a change that would seed them: a row whose file has no probe
// never carries one, and is not re-applied on every reconcile for that.
func (c Change) current(seedKey, itemKey, seedVersion string) bool {
	return seedKey == c.SeedKey && itemKey == c.Item.Key() && (!c.seedable() || seedVersion == SeedVersion)
}

// fileChanged reports whether this apply gives an item Plex already shows
// a different file: a row found by its stamp (never one adopted at first
// contact, which would refresh the whole library at the first resync)
// whose path changed, or whose seeded probe did. A change of markers or of
// the item chain alone is not one.
func (f found) fileChanged(have bool, c Change, seeded bool) bool {
	if !have || f.adopted {
		return false
	}
	return f.file != c.PlexPath || (seeded && f.probeHash != c.Input.ProbeHash)
}

// pendingRefresh holds the ids to refresh -- the rows EnsureChain inserted,
// and an existing item whose file changed -- until the transaction commits
// (review I11).
type pendingRefresh struct{ ids []int64 }

func (r *pendingRefresh) NeedsRefresh(id int64) {
	if !slices.Contains(r.ids, id) {
		r.ids = append(r.ids, id)
	}
}

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
