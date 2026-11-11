// Command guidebuild builds a cloud guide from recorded guide-service
// answers into a schema, and compares it with a guide PMS loaded from the
// same answers, column by column: the oracle of plan B in
// docs/superpowers/specs/2026-11-10-guide-writer-and-built-cloud-guide-design.md.
//
// The fixture directory holds what PMS fetched for one load: the lineup's
// stations (lineup-channels.json), each channel's weeks (grid/*.json) and
// each show's and movie's metadata (metadata/<ratingKey>.json). The channels
// built are the reference's channel tags, each its grid key and number.
//
//	go run ./hack/guidebuild -dsn "$(cat dsn)" -fixture ./fixture -ref epg_ref_pms -out epg_ref_built
//
// It prints, per table, the rows only one side has and, per column, how
// many rows the two disagree on, with examples. Ids are compared, and
// foreign keys both as ids and by what they point at; the load's own
// timestamps are not. An item the service dates no addedAt is stamped by PMS with its own
// clock as it writes the item (the second the answer that brought it
// arrived, a few milliseconds later: 87% to the second on the rig, the rest
// the next one), which no build can reproduce and PMS would not repeat; two
// such added_at near the load are the same (loadTime), so a compare alone
// (-compare-only) needs -now, the load's start, as a build does.
//
// -xmltv builds an XMLTV DVR's guide instead, from the XMLTV file PMS
// loaded (pkg/plex/xmltvguide's reader, guidebuild's XMLTV flavour), the
// device's lineup.json (-lineup) choosing the channels as the managers'
// builder does, each by its GuideNumber; without -lineup every channel of
// the guide is built, its id taken as its number:
//
//	go run ./hack/guidebuild -dsn "$(cat dsn)" -xmltv xmltv.xml -lineup lineup.json -ref epg_ref_x1 -out epg_ref_x1_built -now <load start> -additions
//
// A build of clustarr's profile writes more than PMS's own reader keeps
// (clustarr's custom-guides design §2.4, §5.4): the plex guids the guide
// carries, where PMS keys tv.plex.xmltv://, and the images PMS stores none
// of (an episode's still and backdrop, a show's or a movie's backdrop, a
// poster where no icon is). The compare tolerates none of them: they are
// rows only one side has and columns that differ, unless -additions lines
// each plex item up with PMS's by the guid PMS's reader keys it by
// (xmltvguide.PMSGUIDs) and lists them apart, every other column still
// compared; the run is then exact, exit 0, when nothing else differs.
package main

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // PMS's item hash is SHA-1 of the guid; this is not security
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mediactl/clusterplex/pkg/plex/bootstrap"
	"github.com/mediactl/clusterplex/pkg/plex/epgcloud"
	"github.com/mediactl/clusterplex/pkg/plex/guidebuild"
	"github.com/mediactl/clusterplex/pkg/plex/xmltvguide"
)

func main() { os.Exit(run()) }

func run() int {
	dsn := flag.String("dsn", os.Getenv("GUIDEBUILD_DSN"), "PostgreSQL connection string")
	fixture := flag.String("fixture", "", "the recorded answers")
	ref := flag.String("ref", "", "the guide PMS loaded from them")
	out := flag.String("out", "", "the schema to build into (dropped first)")
	library := flag.String("library", "plex", "the library schema the guide's structure is cloned from")
	examples := flag.Int("examples", 3, "examples per differing column")
	compareOnly := flag.Bool("compare-only", false, "compare -out with -ref without building")
	nowFlag := flag.Int64("now", 0, "the build's time, Unix seconds: PMS's load's start, so the same airings are over (default now)")
	xmltvFile := flag.String("xmltv", "", "build an XMLTV DVR's guide from this XMLTV file, in place of -fixture")
	lineupFile := flag.String("lineup", "", "with -xmltv: the device's lineup.json, whose GuideNumbers are the channels built (default: every channel of the guide, by its id)")
	additions := flag.Bool("additions", false, "with -xmltv: list what the build adds to PMS's load (plex guids, images PMS stores none of) apart, and exit 0 when the rest is equal")
	flag.Parse()
	switch {
	case *dsn == "" || *ref == "" || *out == "" || (*fixture == "" && *xmltvFile == "" && !*compareOnly):
		fmt.Fprintln(os.Stderr, "guidebuild: -dsn, -fixture (or -xmltv), -ref and -out are required")
		return 2
	case *fixture != "" && *xmltvFile != "":
		fmt.Fprintln(os.Stderr, "guidebuild: -fixture and -xmltv are two kinds of guide; give one")
		return 2
	case (*additions || *lineupFile != "") && *xmltvFile == "":
		fmt.Fprintln(os.Stderr, "guidebuild: -additions and -lineup read an XMLTV guide (-xmltv)")
		return 2
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "guidebuild:", err)
		return 2
	}
	defer func() { _ = conn.Close(ctx) }()
	// A compare alone knows the load's time only from -now; without it an
	// added_at of PMS's own clock is a difference like any other.
	loadTime = *nowFlag
	now := time.Now()
	if *nowFlag > 0 {
		now = time.Unix(*nowFlag, 0)
	}
	var add *additionSet
	switch {
	case *xmltvFile != "":
		guide, err := os.ReadFile(*xmltvFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guidebuild:", err)
			return 2
		}
		lineup, err := xmltvLineup(*lineupFile, guide)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guidebuild:", err)
			return 2
		}
		if !*compareOnly {
			loadTime = now.Unix()
			if _, err := buildXMLTV(ctx, conn, os.Stdout, guide, lineup, *ref, *out, *library, now); err != nil {
				fmt.Fprintln(os.Stderr, "guidebuild:", err)
				return 2
			}
		}
		if *additions {
			if add, err = xmltvAdditions(guide, lineup, now); err != nil {
				fmt.Fprintln(os.Stderr, "guidebuild:", err)
				return 2
			}
		}
	case !*compareOnly:
		loadTime = now.Unix()
		if err := build(ctx, conn, *fixture, *ref, *out, *library, now); err != nil {
			fmt.Fprintln(os.Stderr, "guidebuild:", err)
			return 2
		}
	}
	differs, err := compare(ctx, conn, os.Stdout, *ref, *out, *examples, add)
	if err != nil {
		fmt.Fprintln(os.Stderr, "guidebuild:", err)
		return 2
	}
	if differs {
		return 1
	}
	return 0
}

// loadTime is the build's time: an added_at of PMS's within a quarter hour
// of it, or of PMS's last request a capture's log records (loadEnd; the
// rig's 248-channel load ran 24 minutes), is PMS's own load time, which the
// build's equals.
var loadTime, loadEnd int64

func build(ctx context.Context, conn *pgx.Conn, fixture, ref, out, library string, now time.Time) error {
	read := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(fixture, name)) }
	body, err := read("lineup-channels.json")
	if err != nil {
		return err
	}
	stations, err := epgcloud.DecodeChannels(body)
	if err != nil {
		return err
	}
	grids := map[string][][]epgcloud.Item{}
	files, _ := filepath.Glob(filepath.Join(fixture, "grid", "*.json"))
	// <gridKey>-<first day>.json, a week each, in the order asked.
	sort.Slice(files, func(i, j int) bool {
		di, dj := strings.SplitN(filepath.Base(files[i]), "-", 2)[1], strings.SplitN(filepath.Base(files[j]), "-", 2)[1]
		return di < dj || (di == dj && files[i] < files[j])
	})
	asked := askedAt(filepath.Join(fixture, "capture.log"))
	for _, t := range asked {
		loadEnd = max(loadEnd, t.Unix())
	}
	readAt := map[string][]time.Time{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		items, err := epgcloud.DecodeItems(b)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		key := strings.SplitN(filepath.Base(f), "-", 2)[0]
		grids[key] = append(grids[key], items)
		readAt[key] = append(readAt[key], asked[filepath.Base(f)])
	}
	if len(asked) == 0 {
		readAt = nil
	}
	meta := map[string]epgcloud.Item{}
	files, _ = filepath.Glob(filepath.Join(fixture, "metadata", "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		items, err := epgcloud.DecodeItems(b)
		if err != nil || len(items) == 0 {
			continue
		}
		meta[strings.TrimSuffix(filepath.Base(f), ".json")] = items[0]
	}
	var channels []guidebuild.Channel
	rows, err := conn.Query(ctx, fmt.Sprintf(`SELECT extra_data::json->>'at:gridKey', coalesce(extra_data::json->>'at:channelVcn', '')
		FROM %s.tags WHERE tag_type = 310 ORDER BY id`, pgx.Identifier{ref}.Sanitize()))
	if err != nil {
		return err
	}
	for rows.Next() {
		var c guidebuild.Channel
		if err := rows.Scan(&c.GridKey, &c.VCN); err != nil {
			return err
		}
		channels = append(channels, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	lineup := ""
	if len(stations) > 0 {
		lineup, _, _ = strings.Cut(stations[0].ID, "-")
	}
	in := guidebuild.Input{
		Lineup: lineup, Stations: stations, Channels: channels, Grids: grids, Metadata: meta, Now: now, ReadAt: readAt,
	}
	_, err = install(ctx, conn, os.Stdout, in, ref, out, library, fmt.Sprintf("%d channels' grids, %d items' metadata", len(grids), len(meta)))
	return err
}

// install builds in into out, a fresh clone of the library schema holding
// ref's sections, which are the build's: Movies, Shows and Sports, and an
// XMLTV DVR's News.
func install(ctx context.Context, conn *pgx.Conn, w io.Writer, in guidebuild.Input, ref, out, library, what string) (guidebuild.Stats, error) {
	if err := conn.QueryRow(ctx, fmt.Sprintf(`SELECT
		coalesce(max(id) FILTER (WHERE name = 'Movies'), 0), coalesce(max(id) FILTER (WHERE name = 'Shows'), 0),
		coalesce(max(id) FILTER (WHERE name = 'Sports'), 0), coalesce(max(id) FILTER (WHERE name = 'News'), 0)
		FROM %s.library_sections`, pgx.Identifier{ref}.Sanitize())).Scan(&in.Sections.Movies, &in.Sections.Shows, &in.Sections.Sports, &in.Sections.News); err != nil {
		return guidebuild.Stats{}, err
	}
	if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{out}.Sanitize()+" CASCADE"); err != nil {
		return guidebuild.Stats{}, err
	}
	if err := bootstrap.CloneSchema(ctx, conn, library, out); err != nil {
		return guidebuild.Stats{}, fmt.Errorf("clone %s: %w", library, err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return guidebuild.Stats{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s.library_sections SELECT * FROM %[2]s.library_sections`,
		pgx.Identifier{out}.Sanitize(), pgx.Identifier{ref}.Sanitize())); err != nil {
		return guidebuild.Stats{}, fmt.Errorf("copy library_sections: %w", err)
	}
	start := time.Now()
	st, err := guidebuild.Build(ctx, tx, out, in)
	if err != nil {
		return guidebuild.Stats{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return guidebuild.Stats{}, err
	}
	s, _ := json.Marshal(st)
	_, _ = fmt.Fprintf(w, "built %s in %s: %s (%s)\n", out, time.Since(start).Round(time.Millisecond), s, what)
	return st, nil
}

// xmltvLineup is the channels an XMLTV build reads, each id to its number:
// the GuideNumbers of the device's lineup.json at path, as the managers'
// builder reads them (each the id of its channel in the guide clustarr
// renders), else every channel of guide by its id.
func xmltvLineup(path string, guide []byte) (map[string]string, error) {
	out := map[string]string{}
	if path == "" {
		_, chans, err := xmltvguide.Read(bytes.NewReader(guide), nil, time.Now())
		if err != nil {
			return nil, err
		}
		for _, c := range chans {
			out[c.ID] = c.ID
		}
		return out, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		GuideNumber string `json:"GuideNumber"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, e := range entries {
		if n := strings.TrimSpace(e.GuideNumber); n != "" {
			out[n] = n
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s lists no channel", path)
	}
	return out, nil
}

// buildXMLTV builds the XMLTV guide's DVR guide into out, as the managers'
// builder plans it (xmltvguide.Read, guidebuild's XMLTV flavour) at now.
func buildXMLTV(ctx context.Context, conn *pgx.Conn, w io.Writer, guide []byte, lineup map[string]string, ref, out, library string, now time.Time) (guidebuild.Stats, error) {
	in, chans, err := xmltvguide.Read(bytes.NewReader(guide), lineup, now)
	if err != nil {
		return guidebuild.Stats{}, err
	}
	var off []string
	for _, c := range chans {
		if c.Number == "" {
			off = append(off, c.ID)
		}
	}
	return install(ctx, conn, w, in, ref, out, library, fmt.Sprintf("%d channels, %d off the lineup %v", len(in.Stations), len(off), off))
}

// table is one table's rows keyed as PMS knows them, each row's columns.
type table struct {
	name   string
	query  string // key, row as jsonb; %[1]s the schema
	ignore []string
}

// Every table's ids are compared as well as what they point at: a build
// numbers its rows as PMS's load does, an item by when PMS makes it and a
// tagging by when PMS writes it, deleted ones' ids left unused (2026-11-10).
// Only the load's own clock is not compared.
var tables = []table{
	{
		"metadata_items", `SELECT m.guid, to_jsonb(m) || jsonb_build_object('parent', p.guid) FROM %[1]s.metadata_items m
		LEFT JOIN %[1]s.metadata_items p ON p.id = m.parent_id WHERE m.metadata_type <> 15`,
		[]string{"created_at", "updated_at", "changed_at", "resources_changed_at"},
	},
	{
		"media_items", `SELECT concat_ws('|', mi.channel_id, mi.begins_at), to_jsonb(mi) || jsonb_build_object('item', m.guid,
		'parts', (SELECT count(*) FROM %[1]s.media_parts mp WHERE mp.media_item_id = mi.id))
		FROM %[1]s.media_items mi LEFT JOIN %[1]s.metadata_items m ON m.id = mi.metadata_item_id`,
		[]string{"created_at", "updated_at"},
	},
	{
		"media_parts", `SELECT concat_ws('|', mi.channel_id, mi.begins_at), to_jsonb(mp) FROM %[1]s.media_parts mp
		JOIN %[1]s.media_items mi ON mi.id = mp.media_item_id`,
		[]string{"created_at", "updated_at"},
	},
	{
		"tags", `SELECT concat_ws('|', tag_type, tag, key), to_jsonb(t) FROM %[1]s.tags t`,
		[]string{"created_at", "updated_at"},
	},
	{
		"taggings", `SELECT concat_ws('|', m.guid, t.tag_type, t.tag, t.key, tg."index"), to_jsonb(tg) FROM %[1]s.taggings tg
		JOIN %[1]s.metadata_items m ON m.id = tg.metadata_item_id JOIN %[1]s.tags t ON t.id = tg.tag_id`,
		[]string{"created_at"},
	},
}

// compare compares guide b with a, the reference, in every row, column
// and id, and reports to w. add, when not nil, holds a's additions to a
// reference PMS's XMLTV reader loaded: b's plex items are lined up with
// a's by add's guids, and the additions are listed apart, no difference.
func compare(ctx context.Context, conn *pgx.Conn, w io.Writer, a, b string, examples int, add *additionSet) (bool, error) {
	differs := false
	if add != nil {
		add.found, add.moved = map[string][]string{}, nil
	}
	for _, t := range tables {
		ra, err := load(ctx, conn, t, a)
		if err != nil {
			return false, fmt.Errorf("%s.%s: %w", a, t.name, err)
		}
		rb, err := load(ctx, conn, t, b)
		if err != nil {
			return false, fmt.Errorf("%s.%s: %w", b, t.name, err)
		}
		if add != nil {
			rb = add.rekey(t.name, rb)
		}
		var onlyA, onlyB []string
		cols := map[string][]string{}
		for k, va := range ra {
			vb, ok := rb[k]
			if !ok {
				onlyA = append(onlyA, k)
				continue
			}
			for c, x := range va {
				if slicesContains(t.ignore, c) {
					continue
				}
				if c == "added_at" && loadTime > 0 && nearLoad(x) && nearLoad(vb[c]) {
					continue
				}
				if add != nil && add.allows(t.name, k, c, x, vb[c]) {
					continue
				}
				if y := vb[c]; fmt.Sprint(x) != fmt.Sprint(y) {
					cols[c] = append(cols[c], fmt.Sprintf("%s: %v / %v", k, short(x), short(y)))
				}
			}
		}
		for k := range rb {
			if _, ok := ra[k]; !ok {
				onlyB = append(onlyB, k)
			}
		}
		_, _ = fmt.Fprintf(w, "\n%s: %d / %d rows, %d only in %s, %d only in %s\n", t.name, len(ra), len(rb), len(onlyA), a, len(onlyB), b)
		sort.Strings(onlyA)
		sort.Strings(onlyB)
		for i := 0; i < len(onlyA) && i < examples; i++ {
			_, _ = fmt.Fprintf(w, "  only %s: %s\n", a, onlyA[i])
		}
		for i := 0; i < len(onlyB) && i < examples; i++ {
			_, _ = fmt.Fprintf(w, "  only %s: %s\n", b, onlyB[i])
		}
		names := make([]string, 0, len(cols))
		for c := range cols {
			names = append(names, c)
		}
		sort.Strings(names)
		for _, c := range names {
			ex := cols[c]
			sort.Strings(ex)
			_, _ = fmt.Fprintf(w, "  %-26s %5d rows differ", c, len(ex))
			for i := 0; i < len(ex) && i < examples; i++ {
				_, _ = fmt.Fprintf(w, "\n      %s", ex[i])
			}
			_, _ = fmt.Fprintln(w)
		}
		if len(onlyA)+len(onlyB)+len(cols) > 0 {
			differs = true
		}
	}
	if add != nil {
		add.report(w, examples)
	}
	return differs, nil
}

// additionSet is what a build of an XMLTV guide adds to PMS's own load of
// it by design: the plex guid of an item PMS keys tv.plex.xmltv:// (guids,
// the build's guid to PMS's, xmltvguide.PMSGUIDs), and an image where PMS
// stores none. found is what a compare met, by kind, each as an example.
type additionSet struct {
	guids map[string]string
	found map[string][]string
	// moved is the plex items a compare's rekey of metadata_items moved,
	// each to its PMS guid.
	moved map[string]string
}

// xmltvAdditions is an XMLTV guide's additions: its plex items, each with
// the guid PMS's reader keys it by.
func xmltvAdditions(guide []byte, lineup map[string]string, now time.Time) (*additionSet, error) {
	guids, err := xmltvguide.PMSGUIDs(bytes.NewReader(guide), lineup, now)
	if err != nil {
		return nil, err
	}
	return &additionSet{guids: guids}, nil
}

// addedImages are the image columns a build may fill where PMS's XMLTV
// reader stores none: user_thumb_url (an episode's still; a show's or a
// movie's poster when its first listing has no icon, or the first image a
// later listing carries when its first carries none) and user_art_url
// (every backdrop). allows takes either only where the reference row holds
// none; one PMS filled is compared like any column.
var addedImages = []string{"user_thumb_url", "user_art_url"}

// rekey lines up a built table's rows with the reference's. Over
// metadata_items, which compare reads first, it decides which plex items
// move: in key order, an item moves to the guid PMS's reader keys it by
// unless a built row holds that key already or an earlier item took it,
// and each item that moves, and only those, is a "plex guid" addition.
// Its row then takes that key and guid, and its hash with them; a parent
// reference, an airing's item and a tagging's key follow it, and nothing
// else moves.
func (s *additionSet) rekey(table string, rows map[string]map[string]any) map[string]map[string]any {
	keys := slices.Sorted(maps.Keys(rows))
	if table == "metadata_items" {
		s.moved = map[string]string{}
		taken := map[string]bool{}
		for _, k := range keys {
			taken[k] = true
		}
		for _, k := range keys {
			p, ok := s.guids[k]
			if !ok || taken[p] {
				continue
			}
			taken[p] = true
			s.moved[k] = p
			s.found["plex guid"] = append(s.found["plex guid"], p+" -> "+k)
		}
	}
	moved := func(v any) any {
		if g, ok := v.(string); ok {
			if p, ok := s.moved[g]; ok {
				return p
			}
		}
		return v
	}
	out := make(map[string]map[string]any, len(rows))
	for _, k := range keys {
		v, nk := rows[k], k
		switch table {
		case "metadata_items":
			if p, ok := s.moved[k]; ok {
				nk = p
				v["guid"] = p
				if v["hash"] == itemHash(k) {
					v["hash"] = itemHash(p)
				}
			}
			v["parent"] = moved(v["parent"])
		case "media_items":
			v["item"] = moved(v["item"])
		case "taggings":
			if g, rest, ok := strings.Cut(k, "|"); ok {
				if p, ok := s.moved[g]; ok {
					nk = p + "|" + rest
				}
			}
		}
		out[nk] = v
	}
	return out
}

// allows reports whether a built row's column c, y where the reference
// holds x, is an addition, and records it: an image the reference has none
// of.
func (s *additionSet) allows(table, key, c string, x, y any) bool {
	if table != "metadata_items" || !slicesContains(addedImages, c) {
		return false
	}
	if x != nil && x != "" || y == nil || y == "" {
		return false
	}
	s.found[c] = append(s.found[c], fmt.Sprintf("%s: %v", key, short(y)))
	return true
}

// report lists the additions a compare met, kind by kind.
func (s *additionSet) report(w io.Writer, examples int) {
	_, _ = fmt.Fprintln(w, "\nadditions (the build's beyond PMS's reader, by design; not differences):")
	kinds := make([]string, 0, len(s.found))
	for k := range s.found {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		ex := s.found[k]
		sort.Strings(ex)
		_, _ = fmt.Fprintf(w, "  %-26s %5d rows", k, len(ex))
		for i := 0; i < len(ex) && i < examples; i++ {
			_, _ = fmt.Fprintf(w, "\n      %s", ex[i])
		}
		_, _ = fmt.Fprintln(w)
	}
}

// itemHash is PMS's metadata_items.hash of a guid.
func itemHash(guid string) string {
	sum := sha1.Sum([]byte(guid)) //nolint:gosec // PMS's own item hash
	return hex.EncodeToString(sum[:])
}

func load(ctx context.Context, conn *pgx.Conn, t table, schema string) (map[string]map[string]any, error) {
	rows, err := conn.Query(ctx, fmt.Sprintf(t.query, pgx.Identifier{schema}.Sanitize()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]any{}
	for rows.Next() {
		var k *string
		var v map[string]any
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		if k == nil {
			continue
		}
		out[*k] = v
	}
	return out, rows.Err()
}

// nearLoad reports whether a timestamp is within a quarter hour of the
// build's time, or of PMS's load as its capture recorded it.
func nearLoad(v any) bool {
	f, ok := v.(float64)
	return ok && f > float64(loadTime-900) && f < float64(max(loadTime, loadEnd)+900)
}

func short(v any) string {
	s := fmt.Sprint(v)
	if len(s) > 90 {
		return s[:90] + "…"
	}
	return s
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// askedAt reads a capture's log (capture.log: "PMS-REQ <PMS's time> <file>",
// one line a grid week as PMS asked for it) into when PMS asked for each
// fixture file, PMS's clock in UTC; nothing when there is no log.
func askedAt(path string) map[string]time.Time {
	out := map[string]time.Time{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) != 6 || f[0] != "PMS-REQ" {
			continue
		}
		if t, err := time.ParseInLocation("Jan 2, 2006 15:04:05.000", strings.Join(f[1:5], " "), time.UTC); err == nil {
			out[f[5]] = t
		}
	}
	return out
}
