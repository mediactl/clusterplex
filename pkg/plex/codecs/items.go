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
// which row that is among several is Plex's to say.
const itemsSQL = `SELECT mi.metadata_item_id
FROM media_items mi
JOIN metadata_items md ON md.id = mi.metadata_item_id
WHERE mi.audio_codec = $1
  AND mi.video_codec IN ('h264', 'hevc')
  AND mi.deleted_at IS NULL
  AND md.deleted_at IS NULL
  AND md.metadata_type IN (1, 4)
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
