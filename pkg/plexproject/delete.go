package plexproject

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Delete is spec §7: the MediaFile's media row and its parts are
// soft-deleted the way Plex's scanner does it, and every item up the chain
// left with no live media and no live child follows, so the empty-trash
// CronJob empties them later and a mistaken delete shows in Plex's trash
// first. An item that keeps another version keeps no trashed one beside it
// (clearSuperseded). A UID with no row is nothing to do.
func (p *Projector) Delete(ctx context.Context, section int64, uid string) error {
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := p.now().Unix()

	var media, item int64
	err = tx.QueryRow(ctx, `SELECT id, metadata_item_id FROM media_items WHERE library_section_id = $1 AND deleted_at IS NULL AND extra_data LIKE $2 ORDER BY id LIMIT 1`,
		section, `%"`+FileUIDKey+`":"`+uid+`"%`).Scan(&media, &item)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := retireMedia(ctx, tx, media, now); err != nil {
		return err
	}
	if err := clearSuperseded(ctx, tx, item, now); err != nil {
		return err
	}
	if err := trashIfEmpty(ctx, tx, item, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// retireMedia soft-deletes a media row and its parts, as Plex's scanner
// does when a file is gone from an item: PMS's Empty Trash removes them,
// and with them an item they leave with nothing live.
func retireMedia(ctx context.Context, tx pgx.Tx, media, now int64) error {
	if _, err := tx.Exec(ctx, `UPDATE media_parts SET deleted_at = $2, updated_at = $2 WHERE media_item_id = $1 AND deleted_at IS NULL`, media, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE media_items SET deleted_at = $2, updated_at = $2 WHERE id = $1`, media, now)
	return err
}

// clearSuperseded removes, once item has a live media row, every deleted
// one beside it that the sweep did not take (a swept row is put back when
// its file returns, spec §9): the version an upgrade, a move or a delete
// left. Plex showed such a row as the item's missing file until Empty
// Trash removed it (talos, 2026-10-27: 43 rows, every upgraded episode of
// Delicious in Dungeon among them), and a projected file never comes back
// to its old row, which only a trashed item still needs. The rows go as
// Empty Trash takes them -- streams, part settings, parts, then the row,
// leaving nothing behind (talos, 2026-10-27) -- and the item's updated_at
// moves, as the sweep moves it when an item keeps another file.
func clearSuperseded(ctx context.Context, tx pgx.Tx, item, now int64) error {
	rows, err := tx.Query(ctx, `SELECT id FROM media_items WHERE metadata_item_id = $1 AND deleted_at IS NOT NULL
		AND coalesce(extra_data, '') NOT LIKE '%"`+MissingKey+`":%'
		AND EXISTS (SELECT 1 FROM media_items WHERE metadata_item_id = $1 AND deleted_at IS NULL)`, item)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(ids) == 0 {
		return err
	}
	for _, q := range []string{
		`DELETE FROM media_streams WHERE media_item_id = ANY($1)`,
		`DELETE FROM media_part_settings WHERE media_part_id IN (SELECT id FROM media_parts WHERE media_item_id = ANY($1))`,
		`DELETE FROM media_parts WHERE media_item_id = ANY($1)`,
		`DELETE FROM media_items WHERE id = ANY($1)`,
	} {
		if _, err := tx.Exec(ctx, q, ids); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE metadata_items SET updated_at = $2 WHERE id = $1`, item, now)
	return err
}
