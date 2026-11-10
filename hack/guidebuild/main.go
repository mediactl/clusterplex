// Command guidebuild builds a cloud guide from recorded guide-service
// answers into a schema, and compares it with a guide PMS loaded from the
// same answers, column by column: the oracle of plan B in
// docs/superpowers/specs/2026-11-10-guide-writer-and-built-cloud-guide-design.md.
//
// The fixture directory holds what PMS fetched for one load: the lineup's
// stations (lineup-channels.json), each channel's weeks (grid/*.json) and
// each show's and movie's metadata (metadata/<ratingKey>.json). The channels
// built are the reference's channel tags.
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
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mediactl/clusterplex/pkg/plex/bootstrap"
	"github.com/mediactl/clusterplex/pkg/plex/epgcloud"
	"github.com/mediactl/clusterplex/pkg/plex/guidebuild"
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
	flag.Parse()
	if *dsn == "" || *ref == "" || *out == "" || (*fixture == "" && !*compareOnly) {
		fmt.Fprintln(os.Stderr, "guidebuild: -dsn, -fixture, -ref and -out are required")
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
	if !*compareOnly {
		now := time.Now()
		if *nowFlag > 0 {
			now = time.Unix(*nowFlag, 0)
		}
		loadTime = now.Unix()
		if err := build(ctx, conn, *fixture, *ref, *out, *library, now); err != nil {
			fmt.Fprintln(os.Stderr, "guidebuild:", err)
			return 2
		}
	}
	differs, err := compare(ctx, conn, *ref, *out, *examples)
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
// of it is PMS's own load time, which the build's equals.
var loadTime int64

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
	var channels []string
	rows, err := conn.Query(ctx, fmt.Sprintf(`SELECT extra_data::json->>'at:gridKey' FROM %s.tags WHERE tag_type = 310`, pgx.Identifier{ref}.Sanitize()))
	if err != nil {
		return err
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return err
		}
		channels = append(channels, k)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	lineup := ""
	if len(stations) > 0 {
		lineup, _, _ = strings.Cut(stations[0].ID, "-")
	}
	var sec guidebuild.Sections
	if err := conn.QueryRow(ctx, fmt.Sprintf(`SELECT
		coalesce(max(id) FILTER (WHERE name = 'Movies'), 0), coalesce(max(id) FILTER (WHERE name = 'Shows'), 0),
		coalesce(max(id) FILTER (WHERE name = 'Sports'), 0) FROM %s.library_sections`, pgx.Identifier{ref}.Sanitize())).Scan(&sec.Movies, &sec.Shows, &sec.Sports); err != nil {
		return err
	}

	if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{out}.Sanitize()+" CASCADE"); err != nil {
		return err
	}
	if err := bootstrap.CloneSchema(ctx, conn, library, out); err != nil {
		return fmt.Errorf("clone %s: %w", library, err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s.library_sections SELECT * FROM %[2]s.library_sections`,
		pgx.Identifier{out}.Sanitize(), pgx.Identifier{ref}.Sanitize())); err != nil {
		return fmt.Errorf("copy library_sections: %w", err)
	}
	start := time.Now()
	st, err := guidebuild.Build(ctx, tx, out, guidebuild.Input{
		Lineup: lineup, Stations: stations, Channels: channels, Grids: grids, Metadata: meta, Sections: sec, Now: now,
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s, _ := json.Marshal(st)
	_, _ = fmt.Fprintf(os.Stdout, "built %s in %s: %s (%d channels' grids, %d items' metadata)\n", out, time.Since(start).Round(time.Millisecond), s, len(grids), len(meta))
	return nil
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

func compare(ctx context.Context, conn *pgx.Conn, a, b string, examples int) (bool, error) {
	differs := false
	for _, t := range tables {
		ra, err := load(ctx, conn, t, a)
		if err != nil {
			return false, fmt.Errorf("%s.%s: %w", a, t.name, err)
		}
		rb, err := load(ctx, conn, t, b)
		if err != nil {
			return false, fmt.Errorf("%s.%s: %w", b, t.name, err)
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
		_, _ = fmt.Fprintf(os.Stdout, "\n%s: %d / %d rows, %d only in %s, %d only in %s\n", t.name, len(ra), len(rb), len(onlyA), a, len(onlyB), b)
		sort.Strings(onlyA)
		sort.Strings(onlyB)
		for i := 0; i < len(onlyA) && i < examples; i++ {
			_, _ = fmt.Fprintf(os.Stdout, "  only %s: %s\n", a, onlyA[i])
		}
		for i := 0; i < len(onlyB) && i < examples; i++ {
			_, _ = fmt.Fprintf(os.Stdout, "  only %s: %s\n", b, onlyB[i])
		}
		names := make([]string, 0, len(cols))
		for c := range cols {
			names = append(names, c)
		}
		sort.Strings(names)
		for _, c := range names {
			ex := cols[c]
			sort.Strings(ex)
			_, _ = fmt.Fprintf(os.Stdout, "  %-26s %5d rows differ", c, len(ex))
			for i := 0; i < len(ex) && i < examples; i++ {
				_, _ = fmt.Fprintf(os.Stdout, "\n      %s", ex[i])
			}
			_, _ = fmt.Fprintln(os.Stdout)
		}
		if len(onlyA)+len(onlyB)+len(cols) > 0 {
			differs = true
		}
	}
	return differs, nil
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
// build's time, so of PMS's load.
func nearLoad(v any) bool {
	f, ok := v.(float64)
	return ok && f > float64(loadTime-900) && f < float64(loadTime+900)
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
