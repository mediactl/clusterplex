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
// first. A UID with no row is nothing to do.
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
