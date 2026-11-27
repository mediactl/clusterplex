package plexproject

import (
	"context"
	"fmt"
	"slices"

	"github.com/mediactl/clusterplex/pkg/clustarrobj"
)

// Rating is the two ratings Plex keeps on a movie's or a show's own row
// (metadata_items.rating, the critics'; audience_rating, the audience's),
// in tenths of a point out of ten. 0 is none, written as NULL.
//
// They are the only ratings Plex sorts by: a library's "Critic Rating" and
// "Audience Rating" sorts, a smart collection's, its own Top Rated hub.
// PMS fills neither from a custom provider. It keeps the provider's Rating
// list as the item's badges (taggings of tag_type 316) and leaves both
// columns NULL, whether or not the provider also answers rating and
// audienceRating on the item; a library on a custom agent has no ratings
// source preference; and every metadata refresh sets the two back to NULL
// (a stock PMS 1.43.5.11079, 2026-11-27; talos that day: 1,279 movies and
// 544 shows, none with either). So the projection writes them, and writes
// them again after every refresh ([Projector.ReconcileRatings]).
type Rating struct {
	Critic, Audience int64
}

// AddRatings records o's ratings in want under every guid a row of the item
// may carry (keys): t is TypeMovie or TypeShow, o as the projection reads
// it ([clustarrobj.Object.WithoutSharedPlexIDs]). An item with no rating is
// recorded too: its row is held at none.
func AddRatings(want map[string]Rating, o *clustarrobj.Object, t ItemType, provider string) {
	r := Rating{Critic: o.CriticRating, Audience: o.AudienceRating}
	guid, prior := keys(provider, t, string(o.UID), o.PlexID, o.SharedPlexID)
	want[guid] = r
	if prior != "" {
		if _, taken := want[prior]; !taken {
			want[prior] = r
		}
	}
}

// ReconcileRatings makes the ratings of the section's movies and shows what
// want says, by the guid each row carries (a movie's editions are rows of
// their own under the movie's guid, [BaseGUID]), and reports how many rows
// it wrote. One statement, which writes only the rows that differ: a pass
// that finds every row as it left it writes nothing. A row whose guid want
// does not name is not clustarr's to rate and is left alone.
//
// It is run often (the watcher's RatingsResync) rather than at an apply:
// Plex's refresh of an item, which an apply itself asks for, erases them.
func (p *Projector) ReconcileRatings(ctx context.Context, section int64, want map[string]Rating) (int64, error) {
	if len(want) == 0 {
		return 0, nil
	}
	guids := make([]string, 0, len(want))
	for g := range want {
		guids = append(guids, g)
	}
	slices.Sort(guids)
	critic, audience := make([]*float64, len(guids)), make([]*float64, len(guids))
	for i, g := range guids {
		critic[i], audience[i] = points(want[g].Critic), points(want[g].Audience)
	}
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE metadata_items m SET rating = w.critic, audience_rating = w.audience
		FROM unnest($2::text[], $3::float8[], $4::float8[]) AS w(guid, critic, audience)
		WHERE m.library_section_id = $1 AND m.metadata_type IN ($5, $6) AND m.deleted_at IS NULL
		AND split_part(m.guid, $7, 1) = w.guid
		AND (m.rating IS DISTINCT FROM w.critic OR m.audience_rating IS DISTINCT FROM w.audience)`,
		section, guids, critic, audience, int(TypeMovie), int(TypeShow), editionMarker)
	if err != nil {
		return 0, fmt.Errorf("plexproject: ratings of section %d: %w", section, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("plexproject: ratings of section %d: %w", section, err)
	}
	return tag.RowsAffected(), nil
}

// points is a rating in tenths as the column holds it, nil (NULL) for none.
func points(tenths int64) *float64 {
	if tenths <= 0 {
		return nil
	}
	v := float64(tenths) / 10
	return &v
}
