package plexproject

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mediactl/clusterplex/pkg/plexseed"
)

// Keys in media_items.extra_data beside plexseed's.
const (
	// FileUIDKey names the MediaFile a media row projects (spec §4).
	FileUIDKey = "cp:mediaFile"
	// SeedKeyKey is clustarrwatch.SeedKey as of the last apply, so a
	// reconcile tells a current row from a drifted one without reading
	// every column.
	SeedKeyKey = "cp:seedKey"
)

// found is a media row located by UID, or adopted by path.
type found struct {
	media, part int64
	seedKey     string
	adopted     bool
}

// findMedia looks a MediaFile's row up by its stamp, then adopts a
// stamp-less row at its path (one Plex's scanner made before this design).
func findMedia(ctx context.Context, tx pgx.Tx, section int64, uid, plexPath string) (found, bool, error) {
	var f found
	var extra string
	err := tx.QueryRow(ctx, `SELECT mi.id, mp.id, mi.extra_data FROM media_items mi JOIN media_parts mp ON mp.media_item_id = mi.id
		WHERE mi.library_section_id = $1 AND mi.deleted_at IS NULL AND mi.extra_data LIKE $2 ORDER BY mp.deleted_at NULLS FIRST, mp.id LIMIT 1`,
		section, `%"`+FileUIDKey+`":"`+uid+`"%`).Scan(&f.media, &f.part, &extra)
	if errors.Is(err, pgx.ErrNoRows) {
		f.adopted = true
		err = tx.QueryRow(ctx, `SELECT mi.id, mp.id, mi.extra_data FROM media_items mi JOIN media_parts mp ON mp.media_item_id = mi.id
			WHERE mi.library_section_id = $1 AND mi.deleted_at IS NULL AND mp.deleted_at IS NULL AND mp.file = $2
			AND mi.extra_data NOT LIKE '%"cp:mediaFile":%' ORDER BY mi.id LIMIT 1`, section, plexPath).Scan(&f.media, &f.part, &extra)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return found{}, false, nil
	}
	if err != nil {
		return found{}, false, err
	}
	f.seedKey = extraValue(extra, SeedKeyKey)
	return f, true, nil
}

// extraValue reads one key of plexseed's JSON extra_data.
func extraValue(extra, key string) string {
	return extraMap(extra)[key]
}

func extraMap(extra string) map[string]string {
	var m map[string]string
	if json.Unmarshal([]byte(extra), &m) != nil || m == nil {
		return map[string]string{}
	}
	delete(m, "url") // ExtraData rebuilds it
	return m
}

// upsertMedia writes the identity and placement of c's media row -- the
// item it hangs off, its location, its part's folder, file and size -- and
// returns the seeder's target. The codec columns, the streams and the
// probe-hash stamp are the seeder's (SeedTx), which runs after; the stamps
// go on last (stampMedia), since the seeder rewrites extra_data when it
// seeds.
func upsertMedia(ctx context.Context, tx pgx.Tx, section, location, item, dir int64, f found, have bool, c Change, now time.Time) (plexseed.Target, error) {
	ts := now.Unix()
	if !have {
		if err := tx.QueryRow(ctx, `INSERT INTO media_items (library_section_id, section_location_id, metadata_item_id, size, created_at, updated_at, extra_data, media_analysis_version)
			VALUES ($1, $2, $3, $4, $5, $5, '', $6) RETURNING id`, section, location, item, c.Input.SizeBytes, ts, plexseed.MediaAnalysisVersion).Scan(&f.media); err != nil {
			return plexseed.Target{}, err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO media_parts (media_item_id, directory_id, file, "index", size, created_at, updated_at, extra_data)
			VALUES ($1, $2, $3, 0, $4, $5, $5, '') RETURNING id`, f.media, dir, c.PlexPath, c.Input.SizeBytes, ts).Scan(&f.part); err != nil {
			return plexseed.Target{}, err
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE media_items SET section_location_id = $2, metadata_item_id = $3, size = $4, updated_at = $5, media_analysis_version = $6, deleted_at = NULL WHERE id = $1`,
			f.media, location, item, c.Input.SizeBytes, ts, plexseed.MediaAnalysisVersion); err != nil {
			return plexseed.Target{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE media_parts SET directory_id = $2, file = $3, size = $4, updated_at = $5, deleted_at = NULL WHERE id = $1`,
			f.part, dir, c.PlexPath, c.Input.SizeBytes, ts); err != nil {
			return plexseed.Target{}, err
		}
	}
	if err := countMedia(ctx, tx, item); err != nil {
		return plexseed.Target{}, err
	}
	return plexseed.Target{Part: f.part, Media: f.media, Item: item}, nil
}

// stampMedia records the MediaFile's UID and seed key on the row's
// extra_data, keeping every key the seeder wrote.
func stampMedia(ctx context.Context, tx pgx.Tx, media int64, c Change) error {
	var extra string
	if err := tx.QueryRow(ctx, `SELECT coalesce(extra_data, '') FROM media_items WHERE id = $1`, media).Scan(&extra); err != nil {
		return err
	}
	m := extraMap(extra)
	m[plexseed.SourceKey] = plexseed.SourceValue
	m[FileUIDKey] = c.UID
	m[SeedKeyKey] = c.SeedKey
	if c.Input.ProbeHash != "" {
		m[plexseed.ProbeHashKey] = c.Input.ProbeHash
	}
	_, err := tx.Exec(ctx, `UPDATE media_items SET extra_data = $2 WHERE id = $1`, media, plexseed.ExtraData(m))
	return err
}

// countMedia keeps metadata_items.media_item_count at the item's live rows.
func countMedia(ctx context.Context, tx pgx.Tx, item int64) error {
	_, err := tx.Exec(ctx, `UPDATE metadata_items SET media_item_count = (SELECT count(*) FROM media_items WHERE metadata_item_id = $1 AND deleted_at IS NULL) WHERE id = $1`, item)
	return err
}
