package codecs

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Querier runs a query returning rows: what plexdb.Pool does.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// itemsSQL finds movies and episodes (metadata types 1 and 4) of one media
// whose first audio track is $1 and whose video Client's profile copies,
// newest first. One media only: a transcode is asked for media 0, and
// which row that is among several is Plex's to say. Only an item of a
// library: a Live TV session leaves its programme in this database too
// (section -4, AAC over H.264 in MPEG-TS), newer than any file, and its
// input is the session's own stream, gone with the session (talos,
// 2026-11-16: after the 11079 upgrade the AAC fetch asked for one, PMS
// logged "Error opening input file .../livetv/sessions/...: 404" and
// "Failed to start session", and the fetch failed).
const itemsSQL = `SELECT mi.metadata_item_id
FROM media_items mi
JOIN metadata_items md ON md.id = mi.metadata_item_id
WHERE mi.audio_codec = $1
  AND mi.video_codec IN ('h264', 'hevc')
  AND mi.deleted_at IS NULL
  AND md.deleted_at IS NULL
  AND md.metadata_type IN (1, 4)
  AND md.library_section_id IN (SELECT id FROM library_sections)
  AND NOT EXISTS (SELECT 1 FROM media_items o
    WHERE o.metadata_item_id = mi.metadata_item_id AND o.id <> mi.id AND o.deleted_at IS NULL)
ORDER BY mi.id DESC
LIMIT $2`

// LibraryItems is Items over Plex's library database.
func LibraryItems(db Querier) Items {
	return func(ctx context.Context, codec string, n int) ([]int64, error) {
		rows, err := db.Query(ctx, itemsSQL, codec, n)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[int64])
	}
}
