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
	if _, err := tx.Exec(ctx, `UPDATE media_parts SET deleted_at = $2, updated_at = $2 WHERE media_item_id = $1 AND deleted_at IS NULL`, media, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE media_items SET deleted_at = $2, updated_at = $2 WHERE id = $1`, media, now); err != nil {
		return err
	}
	for id := item; id != 0; {
		var parent *int64
		var live int
		if err := tx.QueryRow(ctx, `SELECT parent_id,
			(SELECT count(*) FROM media_items WHERE metadata_item_id = $1 AND deleted_at IS NULL)
			+ (SELECT count(*) FROM metadata_items WHERE parent_id = $1 AND deleted_at IS NULL)
			FROM metadata_items WHERE id = $1`, id).Scan(&parent, &live); err != nil {
			return err
		}
		if err := countMedia(ctx, tx, id); err != nil {
			return err
		}
		if live > 0 {
			break
		}
		if _, err := tx.Exec(ctx, `UPDATE metadata_items SET deleted_at = $2, updated_at = $2 WHERE id = $1`, id, now); err != nil {
			return err
		}
		if parent == nil {
			break
		}
		id = *parent
	}
	return tx.Commit(ctx)
}
