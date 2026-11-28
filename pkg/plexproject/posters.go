package plexproject

import (
	"context"
	"fmt"
)

const (
	// posterTagType is the tag_type of the tag a downloaded poster hangs
	// on: the tagging's thumb_url names the file in the item's bundle and
	// its text the URL it was downloaded from.
	posterTagType = 312
	// cloudThumb is how a poster through Plex's cloud photo service
	// begins, and downloadedPoster how one in the item's bundle does.
	cloudThumb       = "https://images.plex.tv/%"
	downloadedPoster = "metadata://posters/%"
	// thumbLocked matches the user_fields of a row whose poster the user
	// locked: 9 is the thumb's field among lockedFields.
	thumbLocked = `lockedFields=([0-9]+\|)*9(\||&|$)`
)

// ReconcilePosters has each of the section's movies and shows that PMS left
// on its match result's thumb take the poster it has already downloaded,
// and reports how many rows it wrote.
//
// A match result's thumb becomes the item's, and a refresh's download
// replaces it only when that is a new file (clustarr ADR-0011's 2026-10-26
// amendment). Since the provider answers a match with the artwork's public
// source URL, such a row shows that source poster through Plex's cloud
// photo service (images.plex.tv, 225 x 336): no rating badge, no title
// image, scaled up on every screen, and fetched from Plex's cloud by every
// client that shows it. talos held 70 on 2026-11-28, each with exactly one
// downloaded poster PMS had not selected.
//
// Selecting that poster through PMS's own API
// (PUT /library/metadata/<id>/poster) changes user_thumb_url and updated_at
// and locks the poster's field; the lock would hold the row on that file
// for good, so the repair took it off again. This pass writes what was
// left: the two columns. A row whose poster field the user locked, one
// with no downloaded poster, and one with several are left alone.
//
// It is run with the ratings (the watcher's RatingsResync): a row is stuck
// from its match on, and nothing PMS does later frees it.
func (p *Projector) ReconcilePosters(ctx context.Context, section int64) (int64, error) {
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The two prefixes and the lock pattern are written into the statement,
	// not passed: with a parameter the planner, once pgx's prepared
	// statement moves to a generic plan, guesses how many rows a LIKE keeps
	// and may read all of taggings for a handful of rows. The lock is asked
	// again of the row as it is updated, so one locked since the inner read
	// is left alone.
	tag, err := tx.Exec(ctx, `UPDATE metadata_items m SET user_thumb_url = d.thumb_url, updated_at = $2
		FROM (
			SELECT s.id, min(g.thumb_url) AS thumb_url
			FROM metadata_items s
			JOIN taggings g ON g.metadata_item_id = s.id AND g.thumb_url LIKE '`+downloadedPoster+`'
			JOIN tags t ON t.id = g.tag_id AND t.tag_type = $5
			WHERE s.library_section_id = $1 AND s.metadata_type IN ($3, $4) AND s.deleted_at IS NULL
			AND s.user_thumb_url LIKE '`+cloudThumb+`' AND COALESCE(s.user_fields, '') !~ '`+thumbLocked+`'
			GROUP BY s.id HAVING count(*) = 1
		) d
		WHERE m.id = d.id AND m.user_thumb_url LIKE '`+cloudThumb+`' AND COALESCE(m.user_fields, '') !~ '`+thumbLocked+`'`,
		section, p.Now().Unix(), int(TypeMovie), int(TypeShow), posterTagType)
	if err != nil {
		return 0, fmt.Errorf("plexproject: posters of section %d: %w", section, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("plexproject: posters of section %d: %w", section, err)
	}
	return tag.RowsAffected(), nil
}
