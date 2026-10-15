package plexproject

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Refresher is told the metadata_items id of a row Plex must fill (spec
// §6 step 2): the watcher queues PUT /library/metadata/{id}/refresh.
type Refresher interface {
	NeedsRefresh(itemID int64)
}

// EnsureChain walks it.Chain top down, finding each node by guid within
// section and inserting a missing one under its parent. An inserted row
// carries only what the structure needs, with no hash and no refreshed_at,
// so Plex's refresh fills the rest (the capture doc: Plex writes the
// provider's answer only when its hash differs from the row's). An
// existing row is left as Plex made it. A node whose Plex id clustarr has
// since learned is re-keyed from its custom guid first.
func EnsureChain(ctx context.Context, tx pgx.Tx, section int64, it Item, now time.Time, refresh Refresher) (int64, error) {
	var parent *int64
	for _, n := range it.Chain {
		if n.PriorGUID != "" {
			if err := ReKey(ctx, tx, section, n.PriorGUID, n.GUID); err != nil {
				return 0, fmt.Errorf("plexproject: re-key %s %q: %w", typeNames[n.Type], n.PriorGUID, err)
			}
		}
		id, err := findItem(ctx, tx, section, n.GUID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			id, err = reviveItem(ctx, tx, section, n.GUID, now)
			if errors.Is(err, pgx.ErrNoRows) {
				id, err = insertItem(ctx, tx, section, parent, n, now)
				if err == nil && refresh != nil {
					refresh.NeedsRefresh(id)
				}
			}
		case err == nil:
			err = placeItem(ctx, tx, id, parent, n, now)
		}
		if err != nil {
			return 0, fmt.Errorf("plexproject: %s %q: %w", typeNames[n.Type], n.GUID, err)
		}
		parent = &id
	}
	if parent == nil {
		return 0, errors.New("plexproject: an item with no chain")
	}
	return *parent, nil
}

func findItem(ctx context.Context, tx pgx.Tx, section int64, guid string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NULL ORDER BY id LIMIT 1`, section, guid).Scan(&id)
	return id, err
}

func insertItem(ctx context.Context, tx pgx.Tx, section int64, parent *int64, n Node, now time.Time) (int64, error) {
	var year, index any
	if n.Year != 0 {
		year = n.Year
	}
	if n.Type == TypeSeason || n.Type == TypeEpisode {
		index = n.Index
	}
	added := n.AddedAt
	if added.IsZero() {
		added = now
	}
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO metadata_items
		(library_section_id, parent_id, metadata_type, guid, media_item_count, title, title_sort, "index", year,
		 added_at, created_at, updated_at, extra_data)
		VALUES ($1, $2, $3, $4, 0, $5, $6, $7, $8, $9, $10, $10, '')
		RETURNING id`,
		section, parent, int(n.Type), n.GUID, n.Title, n.TitleSort, index, year, added.Unix(), now.Unix()).Scan(&id)
	return id, err
}

// reviveItem brings a soft-deleted row with the guid back, keeping its id:
// what Plex does when a file returns before the trash is emptied, and what
// keeps collections and playlists keyed by the id across an upgrade whose
// delete landed before its add (review C2).
func reviveItem(ctx context.Context, tx pgx.Tx, section int64, guid string, now time.Time) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `UPDATE metadata_items SET deleted_at = NULL, updated_at = $3
		WHERE id = (SELECT id FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NOT NULL ORDER BY deleted_at DESC, id DESC LIMIT 1)
		RETURNING id`, section, guid, now.Unix()).Scan(&id)
	return id, err
}

// placeItem puts a found row where the chain says: under its parent, at
// its index (review I7: an episode renumbered, a season re-keyed).
func placeItem(ctx context.Context, tx pgx.Tx, id int64, parent *int64, n Node, now time.Time) error {
	var index any
	if n.Type == TypeSeason || n.Type == TypeEpisode {
		index = n.Index
	}
	_, err := tx.Exec(ctx, `UPDATE metadata_items SET parent_id = $2, "index" = $3, updated_at = $4
		WHERE id = $1 AND (parent_id IS DISTINCT FROM $2 OR "index" IS DISTINCT FROM $3)`, id, parent, index, now.Unix())
	return err
}

// trashIfEmpty recounts item and soft-deletes it, and each parent left
// with nothing live, as Delete does.
func trashIfEmpty(ctx context.Context, tx pgx.Tx, item, now int64) error {
	for id := item; id != 0; {
		var parent *int64
		var live int
		if err := tx.QueryRow(ctx, `SELECT parent_id,
			(SELECT count(*) FROM media_items WHERE metadata_item_id = $1 AND deleted_at IS NULL)
			+ (SELECT count(*) FROM metadata_items WHERE parent_id = $1 AND deleted_at IS NULL)
			FROM metadata_items WHERE id = $1`, id).Scan(&parent, &live); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if err := countMedia(ctx, tx, id); err != nil {
			return err
		}
		if live > 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE metadata_items SET deleted_at = $2, updated_at = $2 WHERE id = $1`, id, now); err != nil {
			return err
		}
		if parent == nil {
			return nil
		}
		id = *parent
	}
	return nil
}

// ReKey renames a node's guid in place once clustarr knows its Plex id
// (Review Focus 1). Watched state in metadata_item_settings is keyed by
// guid too, so it moves with the row; a row already carrying the new guid,
// or none carrying the old, is a no-op.
func ReKey(ctx context.Context, tx pgx.Tx, section int64, from, to string) error {
	if from == to || from == "" {
		return nil
	}
	tag, err := tx.Exec(ctx, `UPDATE metadata_items SET guid = $3, updated_at = $4 WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NULL`,
		section, from, to, time.Now().Unix())
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE metadata_item_settings SET guid = $2 WHERE guid = $1`, from, to)
	return err
}
