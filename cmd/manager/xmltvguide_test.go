package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexbootstrap "github.com/mediactl/clusterplex/pkg/plex/bootstrap"
	"github.com/mediactl/clusterplex/pkg/plex/epgcloud"
	"github.com/mediactl/clusterplex/pkg/plex/guidebuild"
)

// xmltvGuide is the XMLTV DVR the tests build.
var xmltvGuide = plexbootstrap.Guide{Provider: guidebuild.ProviderXMLTV, DVR: testDVR}

// xmltvBody is a block's XMLTV as clustarr's tuner serves it, each channel
// known by its number: a programme on 1001 starting at start, an hour each
// of n, and one on 1002, a channel the device's lineup does not list.
func xmltvBody(start time.Time, n int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<tv>\n")
	b.WriteString(`<channel id="1001"><display-name>WABC</display-name></channel>` + "\n")
	b.WriteString(`<channel id="1002"><display-name>CNN</display-name></channel>` + "\n")
	for i := range n {
		s := start.Add(time.Duration(i) * time.Hour).UTC().Format("20060102150405 -0700")
		e := start.Add(time.Duration(i+1) * time.Hour).UTC().Format("20060102150405 -0700")
		fmt.Fprintf(&b, `<programme start="%s" stop="%s" channel="1001"><title>Evening Edition</title>`+
			`<sub-title>Part %d</sub-title><episode-num system="xmltv_ns">1.%d.</episode-num><category>News</category></programme>`+"\n", s, e, i+1, i)
		fmt.Fprintf(&b, `<programme start="%s" stop="%s" channel="1002"><title>Off the lineup</title></programme>`+"\n", s, e)
	}
	b.WriteString("</tv>\n")
	return b.String()
}

// tuner serves a device's lineup.json (1001 alone) and its xmltv.xml, the
// body *guide holds at each request, at /hdhr/<id>.
func tuner(t *testing.T, guide *string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hdhr/1AED97B8/lineup.json":
			_, _ = io.WriteString(w, `[{"GuideNumber":"1001","GuideName":"WABC","URL":"stream/1001"}]`)
		case "/hdhr/1AED97B8/xmltv.xml":
			_, _ = io.WriteString(w, *guide)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/hdhr/1AED97B8/xmltv.xml"
}

// xmltvHolder is a test manager holding the plex.tv Lease, with the XMLTV
// guides built by the managers.
func xmltvHolder(t *testing.T) *Manager {
	t.Helper()
	m := newTestManager()
	m.Config.PlexDir = t.TempDir()
	m.Config.LiveTV = LiveTVConfig{Enabled: true, CloudGuide: CloudGuidePMS, XMLTVGuide: XMLTVGuideManager, CloudGuideBuild: CloudGuideBuildInProcess}
	require.NoError(t, os.MkdirAll(m.guideDir(), 0o755))
	m.mu.Lock()
	m.stopLeader = func() {}
	m.mu.Unlock()
	m.xmltv = newXMLTVGuides(m)
	return m
}

// TestAHashMoveInsideTheFloorWaits: clustarr renders a block's XMLTV again
// on every change of a guide source, so a moved hash is built at most every
// xmltvBuildFloor; an unmoved one daily; one never built here at once; a
// failed one after cloudRetryAfter.
func TestAHashMoveInsideTheFloorWaits(t *testing.T) {
	now := time.Now()
	x := newXMLTVGuides(newTestManager())
	w := func(hash string) xmltvWant {
		return xmltvWant{dvr: testDVR, url: "http://tuner/hdhr/1AED97B8/xmltv.xml", hash: hash}
	}
	assert.True(t, x.due(w("h1"), now), "never built by this holder: at once")

	x.built[strings.ToLower(testDVR)] = cloudBuilt{hash: "h1", at: now}
	assert.False(t, x.due(w("h2"), now.Add(10*time.Minute)), "a hash move ten minutes after a build waits")
	assert.False(t, x.due(w("h2"), now.Add(xmltvBuildFloor-time.Second)))
	assert.True(t, x.due(w("h2"), now.Add(xmltvBuildFloor)), "and is built once the floor has passed")
	assert.False(t, x.due(w("h1"), now.Add(23*time.Hour)), "an unmoved hash waits a day")
	assert.True(t, x.due(w("h1"), now.Add(xmltvBuildEvery)))

	x.built[strings.ToLower(testDVR)] = cloudBuilt{hash: "h1", at: now.Add(-2 * time.Hour)}
	require.True(t, x.due(w("h2"), now))
	x.failed(testDVR, now)
	assert.False(t, x.due(w("h2"), now.Add(time.Minute)), "a failure is not tried again at the next poll")
	assert.True(t, x.due(w("h2"), now.Add(cloudRetryAfter)), "but after the wait")
	assert.Equal(t, now.Add(-2*time.Hour), x.built[strings.ToLower(testDVR)].at, "the installed guide keeps its age")
}

// TestAWantedDVRIsKnownByItsUUIDInAnyCase: PMS names a DVR's uuid one way
// and its guide file another; a want for a new hash wakes the builder.
func TestAWantedDVRIsKnownByItsUUIDInAnyCase(t *testing.T) {
	x := newXMLTVGuides(newTestManager())
	assert.False(t, x.wanted(testDVR))
	assert.False(t, (*xmltvGuides)(nil).wanted(testDVR), "no builder wants nothing")
	x.Want(testDVR, "http://tuner/hdhr/1AED97B8/xmltv.xml", "h1")
	assert.True(t, x.wanted(strings.ToUpper(testDVR)))
	assert.True(t, x.wanted(strings.ToLower(testDVR)))
	<-x.kick
	x.Want(testDVR, "http://tuner/hdhr/1AED97B8/xmltv.xml", "h1")
	select {
	case <-x.kick:
		t.Fatal("the same want again wakes the builder")
	default:
	}
	x.Want(testDVR, "http://tuner/hdhr/1AED97B8/xmltv.xml", "h2")
	select {
	case <-x.kick:
	default:
		t.Fatal("a moved hash does not wake the builder")
	}
	x.Want("", "http://tuner/x", "h")
	x.Want("16cac8a1-0746-4e63-9a32-c9d0fd96a0c1", "", "h")
	assert.Len(t, x.wants, 1, "a want without a DVR or a URL is none")
}

// TestTheFirstLoadIsImportedAndTheNextIsSkipped: PMS's first load of a new
// XMLTV DVR's guide pairs its channel map, so the guide socket takes it as
// it takes any load; once the converger has named the DVR, every load of
// it is refused at its open and its rename, and the poll copies none.
func TestTheFirstLoadIsImportedAndTheNextIsSkipped(t *testing.T) {
	m := xmltvHolder(t)
	// The library, when there is one, is the server's before it serves.
	withDB := os.Getenv("CLUSTERPLEX_TEST_POSTGRES_DSN") != ""
	if withDB {
		m.Config.Postgres = libraryDatabase(t)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.Config.GuideSocket = filepath.Join(t.TempDir(), "g.sock")
	ln, err := m.listenGuideSocket()
	require.NoError(t, err)
	go m.serveGuideSocket(ctx, ln)
	ask := func(line string) string {
		c, err := net.Dial("unix", m.Config.GuideSocket)
		require.NoError(t, err)
		defer func() { _ = c.Close() }()
		_, err = fmt.Fprintln(c, line)
		require.NoError(t, err)
		answer, err := bufio.NewReader(c).ReadString('\n')
		require.NoError(t, err)
		return strings.TrimSpace(answer)
	}
	load := filepath.Join(m.guideDir(), xmltvGuide.LoadFile())

	assert.Empty(t, m.loadsNotOurs(xmltvGuide), "before the converger names it, the guide's loads are the holder's")
	t.Run("PMS's first load is imported", func(t *testing.T) {
		if !withDB {
			t.Skip("CLUSTERPLEX_TEST_POSTGRES_DSN is not set")
		}
		db := m.Config.Postgres
		writeGuideLoad(t, db, load, 4, time.Unix(1792540000, 0))
		assert.Equal(t, "ok", ask("swap "+load))
		conn := guideConn(t, db.DSN())
		assert.Equal(t, 4, one(t, conn, "SELECT count(*) FROM "+xmltvGuide.Schema()+".media_items"))
	})

	m.xmltv.Want(testDVR, "http://tuner/hdhr/1AED97B8/xmltv.xml", "h1")
	for _, verb := range []string{"stage", "swap"} {
		assert.Equal(t, "skip the managers build this guide", ask(verb+" "+load), verb)
	}
	other := plexbootstrap.Guide{Provider: guidebuild.ProviderXMLTV, DVR: "16cac8a1-0746-4e63-9a32-c9d0fd96a0c1"}
	assert.Empty(t, m.loadsNotOurs(other), "a DVR the converger has not named")
	cloud := plexbootstrap.Guide{Provider: cloudGuideProvider, DVR: testDVR}
	assert.Empty(t, m.loadsNotOurs(cloud), "a cloud guide PMS fills, whatever its DVR")
}

// guideConn is a connection to dsn, closed with the test.
func guideConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	conn, err := plexbootstrap.ConnectWith(cfg)(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// TestAnEmptyGuideIsNeverBuilt: an XMLTV with nothing left to air on the
// device's channels -- no programme, only ended ones, or only a channel the
// lineup does not list -- is refused, and the builder records the failure,
// leaving the DVR the guide it has.
func TestAnEmptyGuideIsNeverBuilt(t *testing.T) {
	now := time.Now().UTC()
	lineup := map[string]string{"1001": "1001"}
	for name, body := range map[string]string{
		"no programme":       xmltvBody(now, 0),
		"only ended ones":    xmltvBody(now.Add(-5*time.Hour), 3),
		"off the lineup":     strings.ReplaceAll(xmltvBody(now, 3), `channel="1001"`, `channel="1003"`),
		"not a guide at all": "<tv><programme",
	} {
		_, err := readXMLTV([]byte(body), lineup, now)
		assert.Error(t, err, name)
	}
	in, err := readXMLTV([]byte(xmltvBody(now.Add(-30*time.Minute), 3)), lineup, now)
	require.NoError(t, err, "a programme on now")
	assert.Equal(t, guidebuild.ProviderXMLTV, in.Provider)
	assert.Equal(t, now, in.Now)

	t.Run("the builder records the failure and the live guide stays", func(t *testing.T) {
		db := libraryDatabase(t)
		m := xmltvHolder(t)
		m.Config.Postgres = db
		conn := guideConn(t, db.DSN())
		quiet := m.Logger
		require.NoError(t, plexbootstrap.InstallBuilt(t.Context(), quiet, conn, "plex", xmltvGuide, func(ctx context.Context, tx pgx.Tx, staging string) error {
			_, err := tx.Exec(ctx, "INSERT INTO "+pgx.Identifier{staging, "media_items"}.Sanitize()+" (metadata_item_id) VALUES (1), (1)")
			return err
		}))
		empty := xmltvBody(now, 0)
		url := tuner(t, &empty)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go m.xmltv.runBuilder(ctx)
		m.xmltv.Want(testDVR, url, "h1")
		require.Eventually(t, func() bool {
			m.xmltv.mu.Lock()
			defer m.xmltv.mu.Unlock()
			return !m.xmltv.built[strings.ToLower(testDVR)].failed.IsZero()
		}, 30*time.Second, 50*time.Millisecond)
		cancel()
		assert.Equal(t, 2, one(t, conn, "SELECT count(*) FROM "+xmltvGuide.Schema()+".media_items"), "the guide the DVR had")
	})
}

// TestAnXMLTVGuideIsBuiltFromTheTunersXMLTV: the holder reads the device's
// lineup.json and the XMLTV PMS loads, caches the XMLTV and its plan, and
// installs the guide as a built one; a later build prunes the XMLTV it
// replaced.
func TestAnXMLTVGuideIsBuiltFromTheTunersXMLTV(t *testing.T) {
	db := libraryDatabase(t)
	m := xmltvHolder(t)
	m.Config.Postgres = db
	conn := guideConn(t, db.DSN())
	ctx := t.Context()
	body := xmltvBody(time.Now().Add(-30*time.Minute), 3)
	url := tuner(t, &body)

	require.NoError(t, m.xmltv.build(ctx, xmltvWant{dvr: testDVR, url: url, hash: "h1"}))
	schema := pgx.Identifier{xmltvGuide.Schema()}.Sanitize()
	assert.Equal(t, 3, one(t, conn, "SELECT count(*) FROM "+schema+".media_items"), "the lineup's channel's three airings")
	assert.Equal(t, 1, one(t, conn, "SELECT count(*) FROM "+schema+".tags WHERE tag_type = 310"), "1001 alone: 1002 is off the lineup")
	assert.Equal(t, 1, one(t, conn, "SELECT count(*) FROM "+schema+".library_sections WHERE name = 'News'"))
	at, err := plexbootstrap.GuideBuiltAt(ctx, conn, xmltvGuide)
	require.NoError(t, err)
	assert.False(t, at.IsZero(), "the guide is a built one")
	built, err := m.builtGuides(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{xmltvGuide.Schema()}, built)
	assert.Equal(t, "h1", m.xmltv.built[strings.ToLower(testDVR)].hash)

	plan, _, ok, err := cachedPlan(ctx, conn, testDVR)
	require.NoError(t, err)
	require.True(t, ok)
	var p xmltvPlan
	require.NoError(t, json.Unmarshal(plan, &p))
	assert.Equal(t, xmltvPlan{Provider: guidebuild.ProviderXMLTV, DVR: testDVR, URL: url, Hash: "h1", Lineup: map[string]string{"1001": "1001"}}, p)

	time.Sleep(10 * time.Millisecond) // the next fetch is later than the first
	require.NoError(t, m.xmltv.build(ctx, xmltvWant{dvr: testDVR, url: url, hash: "h2"}))
	keys, err := epgcloud.CacheList(ctx, conn, xmltvPrefix)
	require.NoError(t, err)
	assert.Equal(t, []string{xmltvKey(testDVR, "h2")}, mapKeys(keys), "the XMLTV the last build read, alone")
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestBuiltGuidesListsXMLTVSchemas: the holder's writer file holds PMS's
// writes to every guide a build installed, an XMLTV DVR's as a cloud
// lineup's, and to no XMLTV guide PMS loaded.
func TestBuiltGuidesListsXMLTVSchemas(t *testing.T) {
	db := libraryDatabase(t)
	m := xmltvHolder(t)
	m.Config.Postgres = db
	conn := guideConn(t, db.DSN())
	ctx := t.Context()
	loaded := plexbootstrap.Guide{Provider: guidebuild.ProviderXMLTV, DVR: "16cac8a1-0746-4e63-9a32-c9d0fd96a0c1"}
	_, err := plexbootstrap.EnsureGuideSchema(ctx, conn, "plex", loaded)
	require.NoError(t, err)
	require.NoError(t, plexbootstrap.InstallBuilt(ctx, m.Logger, conn, "plex", xmltvGuide,
		func(context.Context, pgx.Tx, string) error { return nil }))

	require.NoError(t, m.writeGuideWriter(ctx, true))
	assert.Equal(t, "*\n-"+xmltvGuide.Schema()+"\n", readGuideWriter(t, m))
}

// TestAGuideBuildJobReadsAnXMLTVPlanAsXMLTV: a build Job reads the plan the
// holder cached by its provider: an XMLTV plan's XMLTV, any other plan a
// cloud lineup's answers, as every plan before XMLTV plans was.
func TestAGuideBuildJobReadsAnXMLTVPlanAsXMLTV(t *testing.T) {
	xmltvPlanBody, err := json.Marshal(xmltvPlan{Provider: guidebuild.ProviderXMLTV, DVR: testDVR, Hash: "h1", Lineup: map[string]string{"1001": "1001"}})
	require.NoError(t, err)
	cloudPlanBody, err := json.Marshal(cloudPlan{DVR: testDVR, Lineup: "lineup1", Channels: []guidebuild.Channel{{GridKey: "k1"}}})
	require.NoError(t, err)
	assert.Equal(t, guidebuild.ProviderXMLTV, planProvider(xmltvPlanBody))
	assert.Equal(t, cloudGuideProvider, planProvider(cloudPlanBody), "a plan naming no provider is a cloud plan")
	assert.Equal(t, cloudGuideProvider, planProvider([]byte(`{"provider":""}`)))

	t.Run("the Job stages the XMLTV plan's guide", func(t *testing.T) {
		db := libraryDatabase(t)
		conn := guideConn(t, db.DSN())
		ctx := t.Context()
		cfg := conn.Config()
		for k, v := range map[string]string{
			"CLUSTERPLEX_POSTGRES_HOST": cfg.Host, "CLUSTERPLEX_POSTGRES_PORT": strconv.Itoa(int(cfg.Port)),
			"CLUSTERPLEX_POSTGRES_DATABASE": cfg.Database, "CLUSTERPLEX_POSTGRES_USER": cfg.User,
			"CLUSTERPLEX_POSTGRES_PASSWORD": cfg.Password, "CLUSTERPLEX_POSTGRES_SCHEMA": "plex",
		} {
			t.Setenv(k, v)
		}
		require.NoError(t, epgcloud.EnsureCache(ctx, conn))
		require.NoError(t, epgcloud.CachePut(ctx, conn, planPrefix+testDVR, xmltvPlanBody, time.Now()))
		stamp := cloudPlanStamp(xmltvPlanBody)
		assert.Equal(t, 1, runGuideBuild([]string{"--dvr", testDVR, "--stamp", stamp}), "no XMLTV cached")

		require.NoError(t, epgcloud.CachePut(ctx, conn, xmltvKey(testDVR, "h1"), []byte(xmltvBody(time.Now(), 0)), time.Now()))
		assert.Equal(t, 1, runGuideBuild([]string{"--dvr", testDVR, "--stamp", stamp}), "an XMLTV with nothing to air")
		have, _, err := plexbootstrap.StagedBuild(ctx, conn, xmltvGuide)
		require.NoError(t, err)
		assert.Empty(t, have, "nothing staged")

		require.NoError(t, epgcloud.CachePut(ctx, conn, xmltvKey(testDVR, "h1"), []byte(xmltvBody(time.Now(), 2)), time.Now()))
		assert.Equal(t, 0, runGuideBuild([]string{"--dvr", testDVR, "--stamp", stamp}))
		have, _, err = plexbootstrap.StagedBuild(ctx, conn, xmltvGuide)
		require.NoError(t, err)
		assert.Equal(t, stamp, have)
		staging := pgx.Identifier{xmltvGuide.Staging()}.Sanitize()
		assert.Equal(t, 2, one(t, conn, "SELECT count(*) FROM "+staging+".media_items"))
		assert.Equal(t, 4, one(t, conn, "SELECT max(library_section_id) FROM "+staging+".metadata_items WHERE metadata_type = 4"),
			"a News episode is filed in the News section")
	})
}
