package plexproject

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Refresher is told the metadata_items id of a row Plex must fill (spec
// §6 step 2): the watcher queues PUT /library/metadata/{id}/refresh.
type Refresher interface {
	NeedsRefresh(itemID int64)
}

// EnsureChain walks it.Chain top down, finding each node's row within
// section ([locate]) and inserting a missing one under its parent. An
// inserted row carries only what the structure needs, with no hash and no
// refreshed_at, so Plex's refresh fills the rest (the capture doc: Plex
// writes the provider's answer only when its hash differs from the row's).
// An existing row is left as Plex made it but for its key: it takes the
// node's guid and edition ([keyItem]), and a trashed one is revived with
// its id. An inserted episode asks for its season's refresh
// ([refreshTarget]), once. It returns the leaf's row, which the media
// hangs off, and the first node's (the movie's or the show's), which
// carries the item's labels.
func EnsureChain(ctx context.Context, tx pgx.Tx, section int64, it Item, now time.Time, refresh Refresher) (leaf, root int64, err error) {
	var parent *int64
	var asked int64
	for i, n := range it.Chain {
		row, err := locate(ctx, tx, section, parent, n)
		if i == len(it.Chain)-1 && it.hint != 0 && (errors.Is(err, pgx.ErrNoRows) || (err == nil && row.trashed && row.id != it.hint)) {
			// No live row carries the item's guid or place, but the file's
			// own row is this item's: it is kept, as it is, whatever guid
			// Plex has written on it since -- a Plex id that changed made a
			// second row and left the first in the trash (talos,
			// 2026-10-27).
			if hinted, herr := liveItem(ctx, tx, section, it.hint, n.Type); herr == nil {
				row, err = hinted, nil
			}
		}
		id := row.id
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			id, err = insertItem(ctx, tx, section, parent, n, now)
			if err == nil && refresh != nil {
				target := id
				if n.Type == TypeEpisode && parent != nil {
					target = *parent
				}
				if target != asked {
					refresh.NeedsRefresh(target)
					asked = target
				}
			}
		case err == nil:
			if err = keyItem(ctx, tx, row, n, now); err == nil {
				err = placeItem(ctx, tx, id, parent, n, now)
			}
		}
		if err != nil {
			return 0, 0, fmt.Errorf("plexproject: %s %q: %w", typeNames[n.Type], n.GUID, err)
		}
		if i == 0 {
			root = id
		}
		parent = &id
	}
	if parent == nil {
		return 0, 0, errors.New("plexproject: an item with no chain")
	}
	return *parent, root, nil
}

// refreshTarget is the row whose Plex refresh fills item: an episode's
// season, the row itself otherwise. PMS cannot refresh a lone plex://
// episode -- it fails building the match ("Missing parameters for match
// request") and asks the provider nothing -- and fills a season's episodes
// from the season's includeChildren=1 answer (talos, 2026-10-22: 128 new
// Monster and Love is Blind episodes were refreshed and got no metadata).
func refreshTarget(ctx context.Context, tx pgx.Tx, item int64) (int64, error) {
	var typ ItemType
	var parent *int64
	if err := tx.QueryRow(ctx, `SELECT metadata_type, parent_id FROM metadata_items WHERE id = $1`, item).Scan(&typ, &parent); err != nil {
		return 0, err
	}
	if typ == TypeEpisode && parent != nil {
		return *parent, nil
	}
	return item, nil
}

// itemRow is the metadata_items row a node was found on.
type itemRow struct {
	id      int64
	guid    string
	edition string
	trashed bool
}

// locate finds the row a node is on, a live one before a trashed one, by
// every key the row may carry: the node's guid or the custom form it had
// before its Plex id was known, a movie's with or without an edition, and
// a season's or an episode's place under its parent. Plex's refresh writes
// whatever guid the provider answers, and Plex's own scan keyed an edition
// as <guid>/edition/<title>, so a row looked up by one exact guid was
// missed whenever the two had drifted, and its item got a second row. On
// talos 73 movies (2026-10-16) and 454 episodes (2026-10-22 to 27) were
// left in Plex's trash that way beside their twins.
func locate(ctx context.Context, tx pgx.Tx, section int64, parent *int64, n Node) (itemRow, error) {
	keys := []string{BaseGUID(n.GUID)}
	if n.PriorGUID != "" {
		keys = append(keys, BaseGUID(n.PriorGUID))
	}
	const cols = `SELECT id, guid, coalesce(edition_title, ''), deleted_at IS NOT NULL FROM metadata_items`
	var row pgx.Row
	switch {
	case n.Type == TypeMovie:
		// A section holds few movies: its guids are matched without their
		// editions, the node's own guid first.
		row = tx.QueryRow(ctx, cols+` WHERE library_section_id = $1 AND metadata_type = $2 AND split_part(guid::text, '`+editionMarker+`', 1) = ANY($3)
			ORDER BY deleted_at IS NOT NULL, guid::text = $4 DESC, array_position($3, split_part(guid::text, '`+editionMarker+`', 1)), deleted_at DESC, id LIMIT 1`,
			section, int(n.Type), keys, n.GUID)
	case parent != nil && (n.Type == TypeSeason || n.Type == TypeEpisode):
		row = tx.QueryRow(ctx, cols+` WHERE library_section_id = $1 AND metadata_type = $2 AND (guid::text = ANY($3) OR (parent_id = $4 AND "index" = $5))
			ORDER BY deleted_at IS NOT NULL, array_position($3, guid::text) NULLS LAST, deleted_at DESC, id LIMIT 1`,
			section, int(n.Type), keys, *parent, n.Index)
	default:
		row = tx.QueryRow(ctx, cols+` WHERE library_section_id = $1 AND metadata_type = $2 AND guid::text = ANY($3)
			ORDER BY deleted_at IS NOT NULL, array_position($3, guid::text), deleted_at DESC, id LIMIT 1`,
			section, int(n.Type), keys)
	}
	var r itemRow
	err := row.Scan(&r.id, &r.guid, &r.edition, &r.trashed)
	return r, err
}

// liveItem reads the live row id of type t in section.
func liveItem(ctx context.Context, tx pgx.Tx, section, id int64, t ItemType) (itemRow, error) {
	var r itemRow
	err := tx.QueryRow(ctx, `SELECT id, guid, coalesce(edition_title, ''), false FROM metadata_items
		WHERE id = $1 AND library_section_id = $2 AND metadata_type = $3 AND deleted_at IS NULL`, id, section, int(t)).Scan(&r.id, &r.guid, &r.edition, &r.trashed)
	return r, err
}

// keyItem gives a found row the node's guid and edition, and revives it
// if it was trashed, keeping its id: what Plex does when a file returns
// before the trash is emptied, and what keeps collections and playlists
// keyed by the id across an upgrade whose delete landed before its add
// (review C2). A row PMS keyed with the same edition keeps PMS's escaping
// of it. The watched state keyed by the row's old guid moves with it.
func keyItem(ctx context.Context, tx pgx.Tx, r itemRow, n Node, now time.Time) error {
	guid := n.GUID
	if r.edition == n.Edition && BaseGUID(r.guid) == BaseGUID(n.GUID) && strings.Contains(r.guid, editionMarker) == (n.Edition != "") {
		guid = r.guid
	}
	if guid == r.guid && r.edition == n.Edition && !r.trashed {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE metadata_items SET guid = $2, edition_title = NULLIF($3, ''), deleted_at = NULL, updated_at = $4 WHERE id = $1`,
		r.id, guid, n.Edition, now.Unix()); err != nil {
		return err
	}
	if guid == r.guid {
		return nil
	}
	return moveWatchState(ctx, tx, r.guid, guid)
}

// moveWatchState moves the watched state keyed by from to to: Plex keys
// metadata_item_settings by guid, an edition's by its edition guid (the
// capture of 2026-10-27), never by row. An account already keyed by to
// keeps its own.
func moveWatchState(ctx context.Context, tx pgx.Tx, from, to string) error {
	_, err := tx.Exec(ctx, `UPDATE metadata_item_settings s SET guid = $2 WHERE s.guid = $1
		AND NOT EXISTS (SELECT 1 FROM metadata_item_settings t WHERE t.guid = $2 AND t.account_id IS NOT DISTINCT FROM s.account_id)`, from, to)
	return err
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
		(library_section_id, parent_id, metadata_type, guid, edition_title, media_item_count, title, title_sort, "index", year,
		 added_at, created_at, updated_at, extra_data)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), 0, $6, $7, $8, $9, $10, $11, $11, NULL)
		RETURNING id`,
		section, parent, int(n.Type), n.GUID, n.Edition, n.Title, n.TitleSort, index, year, added.Unix(), now.Unix()).Scan(&id)
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
