package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexbootstrap "github.com/mediactl/clusterplex/pkg/plex/bootstrap"
)

// Two guide schemas cloned from the library compare equal, and a row one
// of them holds is reported by its key: the queries name real columns.
func TestCompareReadsTwoGuideSchemas(t *testing.T) {
	dsn := os.Getenv("CLUSTERPLEX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CLUSTERPLEX_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	admin, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	root, err := pgx.ConnectConfig(ctx, admin)
	require.NoError(t, err)
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "guidediff_" + hex.EncodeToString(b[:])
	_, err = root.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	require.NoError(t, root.Close(ctx))
	t.Cleanup(func() {
		c, err := pgx.ConnectConfig(context.Background(), admin)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	cfg := admin.Copy()
	cfg.Database = name
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, plexbootstrap.InitSchema(ctx, quiet, plexbootstrap.ConnectWith(cfg), "plex", filepath.Join("..", "plex-postgresql", "schema")))
	conn, err := pgx.ConnectConfig(ctx, cfg)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	require.NoError(t, plexbootstrap.CloneSchema(ctx, conn, "plex", "epg_a"))
	require.NoError(t, plexbootstrap.CloneSchema(ctx, conn, "plex", "epg_b"))
	for _, s := range []string{"epg_a", "epg_b"} {
		_, err = conn.Exec(ctx, `INSERT INTO `+s+`.metadata_items (id, guid, metadata_type, title, "index") VALUES (1, 'plex://show/1', 2, 'The Show', 0), (2, 'plex://episode/2', 4, 'Pilot', 1)`)
		require.NoError(t, err)
		_, err = conn.Exec(ctx, `UPDATE `+s+`.metadata_items SET parent_id = 1 WHERE id = 2`)
		require.NoError(t, err)
		_, err = conn.Exec(ctx, `INSERT INTO `+s+`.media_items (id, metadata_item_id, channel_id, begins_at, ends_at) VALUES (1, 2, 3, 1000, 2000)`)
		require.NoError(t, err)
		_, err = conn.Exec(ctx, `INSERT INTO `+s+`.media_parts (id, media_item_id) VALUES (1, 1)`)
		require.NoError(t, err)
		_, err = conn.Exec(ctx, `INSERT INTO `+s+`.tags (id, tag, tag_type) VALUES (1, 'Drama', 1)`)
		require.NoError(t, err)
		_, err = conn.Exec(ctx, `INSERT INTO `+s+`.taggings (id, metadata_item_id, tag_id, "index") VALUES (1, 1, 1, 0)`)
		require.NoError(t, err)
	}
	report, differs, err := Compare(ctx, conn, "epg_a", "epg_b", 5)
	require.NoError(t, err)
	assert.False(t, differs, report)

	_, err = conn.Exec(ctx, `UPDATE epg_b.metadata_items SET title = 'The Show (2026)' WHERE id = 1`)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `INSERT INTO epg_b.media_items (id, metadata_item_id, channel_id, begins_at, ends_at) VALUES (2, 2, 3, 2000, 3000)`)
	require.NoError(t, err)
	report, differs, err = Compare(ctx, conn, "epg_a", "epg_b", 5)
	require.NoError(t, err)
	assert.True(t, differs)
	assert.Contains(t, report, "metadata_items by key: 0 only in epg_a, 0 only in epg_b, 1 differ")
	assert.Contains(t, report, "media_items by key: 0 only in epg_a, 1 only in epg_b, 0 differ")
	assert.Contains(t, report, "only in B: 3|2000")
}
