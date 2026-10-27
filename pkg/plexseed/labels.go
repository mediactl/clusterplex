package plexseed

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// LabelTagType is Plex's tag_type for a Label, the tag a user sets on an
// item (verified on the owner's server, 2026-10-27: a label put through
// PUT /library/metadata/<id>?label[0].tag.tag= lands as tag_type 11, its
// tagging at index 0 with no text or extra_data).
const LabelTagType = 11

// labelExtra marks a label tagging as the seeder's; a label the owner set
// in Plex carries none and is never touched.
const labelExtra = `{` + ourMark + `}`

type labelRow struct {
	id    int64
	tag   string
	extra string
}

// ReconcileLabelsTx makes item's labels of ours exactly want: a label of
// ours no longer wanted is removed, a wanted one the item lacks is added
// (under the library's existing tag of that name, else a new one), and
// one the owner set themselves -- the same name included -- is left as it
// is. PMS keeps no Label from a metadata provider, so clustarr's keywords
// reach Plex only this way, and a Plex refresh rewrites none of them.
func (s *Seeder) ReconcileLabelsTx(ctx context.Context, tx pgx.Tx, item int64, want []string, now int64) (written, removed int, err error) {
	rows, err := tx.Query(ctx, `
SELECT g.id, t.tag, coalesce(g.extra_data, '')
  FROM taggings g JOIN tags t ON t.id = g.tag_id AND t.tag_type = $2
 WHERE g.metadata_item_id = $1
 ORDER BY g."index", g.id`, item, LabelTagType)
	if err != nil {
		return 0, 0, fmt.Errorf("plexseed: labels of %d: %w", item, err)
	}
	have, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (labelRow, error) {
		var l labelRow
		return l, r.Scan(&l.id, &l.tag, &l.extra)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("plexseed: labels of %d: %w", item, err)
	}

	desired := make([]string, 0, len(want))
	seen := map[string]bool{}
	for _, w := range want {
		w = strings.TrimSpace(w)
		if w == "" || seen[strings.ToLower(w)] {
			continue
		}
		seen[strings.ToLower(w)] = true
		desired = append(desired, w)
	}
	present := map[string]bool{}
	for _, h := range have {
		key := strings.ToLower(h.tag)
		if strings.Contains(h.extra, ourMark) && !seen[key] {
			if _, err := tx.Exec(ctx, "DELETE FROM taggings WHERE id = $1", h.id); err != nil {
				return written, removed, fmt.Errorf("plexseed: delete label %d: %w", h.id, err)
			}
			removed++
			continue
		}
		present[key] = true
	}
	index := int64(len(have) - removed)
	for _, d := range desired {
		if present[strings.ToLower(d)] {
			continue
		}
		tag, err := labelTag(ctx, tx, d, now)
		if err != nil {
			return written, removed, err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO taggings (metadata_item_id, tag_id, "index", created_at, extra_data)
VALUES ($1, $2, $3, $4, $5)`, item, tag, index, now, labelExtra); err != nil {
			return written, removed, fmt.Errorf("plexseed: insert label on %d: %w", item, err)
		}
		index++
		written++
	}
	return written, removed, nil
}

// labelTag is the library's tag row for the label name, created as Plex
// creates one (tag and type, no key) when there is none.
func labelTag(ctx context.Context, tx pgx.Tx, name string, now int64) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, "SELECT id FROM tags WHERE tag_type = $1 AND tag = $2 ORDER BY id LIMIT 1", LabelTagType, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx,
			"INSERT INTO tags (tag, tag_type, created_at, updated_at) VALUES ($1, $2, $3, $3) RETURNING id", name, LabelTagType, now,
		).Scan(&id)
	}
	if err != nil {
		return 0, fmt.Errorf("plexseed: label tag %q: %w", name, err)
	}
	return id, nil
}
