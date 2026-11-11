package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexbootstrap "github.com/mediactl/clusterplex/pkg/plex/bootstrap"
)

// oracleDB is a connection to a fresh library database, with a schema
// sections holding an XMLTV DVR's four sections as PMS's load makes them.
func oracleDB(t *testing.T) *pgx.Conn {
	t.Helper()
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
	name := "guidebuild_" + hex.EncodeToString(b[:])
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
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	require.NoError(t, plexbootstrap.CloneSchema(ctx, conn, "plex", "sections"))
	_, err = conn.Exec(ctx, `INSERT INTO sections.library_sections (id, name, section_type, language, agent, scanner, uuid)
		VALUES (1, 'Movies', 1, 'en', '', '', 'm'), (2, 'Shows', 2, 'en', '', '', 's'), (3, 'Sports', 2, 'en', '', '', 'p'), (4, 'News', 2, 'en', '', '', 'n')`)
	require.NoError(t, err)
	return conn
}

// TestXMLTVOracleBuildsFromAFile: -xmltv builds a guide from an XMLTV file
// through the reader, -lineup's GuideNumbers choosing the channels (without
// it, every channel of the guide by its id), and compares it with a
// reference in every row and column. What a build adds to PMS's own load
// of the guide (the plex guids, the images PMS's reader stores none of) is
// a difference unless -additions lists it apart.
//
// The reference here stands in for PMS's load: the same guide without its
// <image>s and plex episode-nums, the two things PMS's reader ignores (the
// rig's DVR 8, clustarr's custom-guides design §2.3), built the same way.
// The rig's run against PMS's own load is docs/guide-rig.md's "The XMLTV
// oracle".
func TestXMLTVOracleBuildsFromAFile(t *testing.T) {
	conn := oracleDB(t)
	ctx := context.Background()
	guide, err := os.ReadFile(filepath.Join("..", "..", "pkg", "plex", "xmltvguide", "testdata", "profile.xml"))
	require.NoError(t, err)
	now := time.Date(2026, 11, 10, 23, 30, 0, 0, time.UTC) // before the profile's first programme
	lineupFile := filepath.Join(t.TempDir(), "lineup.json")
	require.NoError(t, os.WriteFile(lineupFile, []byte(`[{"GuideNumber":"9001","GuideName":"X1 9001","URL":"http://t/stream/9001"},
		{"GuideNumber":"9002"},{"GuideNumber":"9003"}]`), 0o600))

	// Without -lineup every channel of the guide is built, 9999 too.
	var out bytes.Buffer
	lineup, err := xmltvLineup("", guide)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"9001": "9001", "9002": "9002", "9003": "9003", "9999": "9999"}, lineup)
	st, err := buildXMLTV(ctx, conn, &out, guide, lineup, "sections", "epg_all", "plex", now)
	require.NoError(t, err)
	assert.Equal(t, 4, st.Channels)

	lineup, err = xmltvLineup(lineupFile, guide)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"9001": "9001", "9002": "9002", "9003": "9003"}, lineup)
	st, err = buildXMLTV(ctx, conn, &out, guide, lineup, "sections", "epg_ours", "plex", now)
	require.NoError(t, err)
	assert.Equal(t, 3, st.Channels, "9999 is not on the device's lineup")
	assert.Equal(t, 1, count(t, conn, `SELECT count(*) FROM epg_ours.library_sections WHERE name = 'News'`), "the reference's sections")
	assert.Equal(t, 4, count(t, conn, `SELECT count(*) FROM epg_ours.library_sections`))
	// A News episode is filed in the reference's News section (the rig's
	// Genres Many).
	news := []byte(`<tv><channel id="9001"><display-name>X1 One</display-name></channel><programme start="20261111000000 +0000" stop="20261111010000 +0000" channel="9001"><title>Nightly</title><category>News</category><episode-num system="xmltv_ns">0.0.</episode-num></programme></tv>`)
	_, err = buildXMLTV(ctx, conn, &out, news, map[string]string{"9001": "9001"}, "sections", "epg_news", "plex", now)
	require.NoError(t, err)
	assert.Equal(t, 4, count(t, conn, `SELECT library_section_id FROM epg_news.metadata_items WHERE guid = 'tv.plex.xmltv://episode/Nightly/1/1'`))

	// The same build compares equal with itself, additions or not.
	differs, err := compare(ctx, conn, &out, "epg_ours", "epg_ours", 3, nil)
	require.NoError(t, err)
	assert.False(t, differs)

	// PMS's view of the guide, and its build.
	pmsView := regexp.MustCompile(`<image[^>]*>[^<]*</image>|<episode-num system="plex(\.show|\.season)?">[^<]*</episode-num>`).ReplaceAll(guide, nil)
	require.NotContains(t, string(pmsView), "plex://")
	_, err = buildXMLTV(ctx, conn, &out, pmsView, lineup, "sections", "epg_pms", "plex", now)
	require.NoError(t, err)

	out.Reset()
	differs, err = compare(ctx, conn, &out, "epg_pms", "epg_ours", 5, nil)
	require.NoError(t, err)
	assert.True(t, differs, "nothing new is tolerated without -additions")
	assert.Contains(t, out.String(), "metadata_items: 29 / 29 rows, 3 only in epg_pms, 3 only in epg_ours")
	assert.Contains(t, out.String(), "only epg_ours: plex://episode/6aeb5f12ac56869440d7304f")
	assert.Regexp(t, `user_art_url\s+\d+ rows differ`, out.String())

	add, err := xmltvAdditions(guide, lineup, now)
	require.NoError(t, err)
	out.Reset()
	differs, err = compare(ctx, conn, &out, "epg_pms", "epg_ours", 5, add)
	require.NoError(t, err)
	assert.False(t, differs, "the rest is equal: %s", out.String())
	report := out.String()
	assert.Contains(t, report, "metadata_items: 29 / 29 rows, 0 only in epg_pms, 0 only in epg_ours")
	assert.Contains(t, report, "additions")
	assert.Regexp(t, `plex guid\s+3 rows`, report)
	assert.Contains(t, report, "tv.plex.xmltv://show/The%20Drew%20Barrymore%20Show -> plex://show/5fc69a487c6557002ed60976")
	// Element Soup's episode still and backdrop, its show's backdrop, the
	// Drew Barrymore show's backdrop, episode still and backdrop, Movie
	// Typical's backdrop.
	assert.Regexp(t, `user_thumb_url\s+2 rows`, report)
	assert.Regexp(t, `user_art_url\s+5 rows`, report)

	// An image PMS does store is compared like any other column.
	_, err = conn.Exec(ctx, `UPDATE epg_ours.metadata_items SET user_thumb_url = 'https://example.org/other.jpg' WHERE guid = 'tv.plex.xmltv://movie/Movie%20Typical'`)
	require.NoError(t, err)
	out.Reset()
	differs, err = compare(ctx, conn, &out, "epg_pms", "epg_ours", 5, add)
	require.NoError(t, err)
	assert.True(t, differs)
	assert.Regexp(t, `user_thumb_url\s+1 rows differ`, out.String())
}

func count(t *testing.T, conn *pgx.Conn, sql string) int {
	t.Helper()
	var n int
	require.NoError(t, conn.QueryRow(context.Background(), sql).Scan(&n))
	return n
}

// rekey moves a built plex item onto PMS's key only when no built row holds
// that key already, the first in key order when two plex items would take
// one; it moves an item's taggings, parent references and airings only with
// the item; and it counts a plex guid addition only for an item it moved:
// the same rows give the same answer every time, whatever Go's map order.
func TestRekeyMovesOnlyWhatItCanAndCountsWhatItMoved(t *testing.T) {
	const (
		ep      = "plex://episode/a"
		twin    = "plex://episode/b" // PMS keys it as ep's PMS key too
		blocked = "plex://episode/c" // PMS's key is a built row already
		pmsEp   = "tv.plex.xmltv://episode/Show/1/1"
		pmsC    = "tv.plex.xmltv://episode/Other/1/1"
	)
	guids := map[string]string{ep: pmsEp, twin: pmsEp, blocked: pmsC}
	items := func() map[string]map[string]any {
		return map[string]map[string]any{
			ep:      {"guid": ep, "hash": itemHash(ep)},
			twin:    {"guid": twin, "hash": itemHash(twin)},
			blocked: {"guid": blocked, "hash": itemHash(blocked)},
			pmsC:    {"guid": pmsC, "hash": itemHash(pmsC)},
			"child": {"guid": "child", "parent": ep},
		}
	}
	taggings := func() map[string]map[string]any {
		return map[string]map[string]any{
			ep + "|310|9001 X1 One||0":        {"id": 1.0},
			twin + "|310|9002 X1 Two||0":      {"id": 2.0},
			blocked + "|310|9003 X1 Three||0": {"id": 3.0},
		}
	}
	for range 50 {
		s := &additionSet{guids: guids, found: map[string][]string{}}
		got := s.rekey("metadata_items", items())
		require.Contains(t, got, pmsEp)
		assert.Equal(t, pmsEp, got[pmsEp]["guid"], "the first plex item in key order moves")
		assert.Equal(t, itemHash(pmsEp), got[pmsEp]["hash"])
		assert.Contains(t, got, twin, "a second plex item for one PMS key keeps its own")
		assert.Contains(t, got, blocked, "a plex item whose PMS key a built row holds keeps its own")
		assert.Equal(t, pmsEp, got["child"]["parent"])
		assert.Equal(t, []string{pmsEp + " -> " + ep}, s.found["plex guid"], "only what moved is an addition")

		tg := s.rekey("taggings", taggings())
		assert.Contains(t, tg, pmsEp+"|310|9001 X1 One||0")
		assert.Contains(t, tg, twin+"|310|9002 X1 Two||0", "a tagging moves only with its item")
		assert.Contains(t, tg, blocked+"|310|9003 X1 Three||0")
		mi := s.rekey("media_items", map[string]map[string]any{"1|100": {"item": ep}, "2|100": {"item": twin}})
		assert.Equal(t, pmsEp, mi["1|100"]["item"])
		assert.Equal(t, twin, mi["2|100"]["item"])
	}
}
