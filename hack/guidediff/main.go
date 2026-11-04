// Command guidediff compares two Live TV guide schemas table by table: the
// oracle of docs/superpowers/specs/2026-11-04-guide-loads-in-postgresql-design.md.
// A guide PMS loaded into a SQLite file and copied in, and the same guide
// PMS loaded through the shim into PostgreSQL, must diff empty on an XMLTV
// lineup from a fixed file; on a cloud lineup the differences allowed are
// the service's own changes at the horizon.
//
// Rows are keyed by what identifies them to PMS, never by id or the load's
// own timestamps: metadata_items by guid, media_items by channel and start,
// tags by type and text, taggings by item guid, tag and index.
//
//	go run ./hack/guidediff -dsn postgres://plex:plex@127.0.0.1:55432/plex -a epg_<uuid>_old -b epg_<uuid>
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

func main() {
	os.Exit(run())
}

// run is main without its exit: 0 for two guides alike, 1 for a
// difference, 2 for a usage or database error.
func run() int {
	dsn := flag.String("dsn", os.Getenv("GUIDEDIFF_DSN"), "PostgreSQL connection string (or GUIDEDIFF_DSN)")
	a := flag.String("a", "", "the first guide schema, e.g. epg_<uuid>_old")
	b := flag.String("b", "", "the second guide schema, e.g. epg_<uuid>")
	examples := flag.Int("examples", 10, "how many differing keys to print per table")
	flag.Parse()
	if *dsn == "" || *a == "" || *b == "" {
		_, _ = fmt.Fprintln(os.Stderr, "guidediff: -dsn, -a and -b are required")
		return 2
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "guidediff:", err)
		return 2
	}
	defer func() { _ = conn.Close(ctx) }()
	report, differs, err := Compare(ctx, conn, *a, *b, *examples)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "guidediff:", err)
		return 2
	}
	_, _ = os.Stdout.WriteString(report)
	if differs {
		return 1
	}
	return 0
}

// keyed is one table's rows keyed by what identifies them to PMS, each
// row's compared columns rendered as one string.
type keyed struct {
	table string
	// query selects key, value from a schema's tables; %[1]s is the
	// schema, sanitized.
	query string
}

var keyedTables = []keyed{
	{"metadata_items", `SELECT m.guid, concat_ws('|', m.metadata_type, m.title, m.title_sort, m."index", m.year, m.duration,
		m.content_rating, m.tags_genre, m.originally_available_at, p.guid, md5(coalesce(m.summary, '')))
		FROM %[1]s.metadata_items m LEFT JOIN %[1]s.metadata_items p ON p.id = m.parent_id WHERE m.guid IS NOT NULL`},
	{"media_items", `SELECT concat_ws('|', mi.channel_id, mi.begins_at), concat_ws('|', mi.ends_at, m.guid, mi.extra_data,
		(SELECT count(*) FROM %[1]s.media_parts mp WHERE mp.media_item_id = mi.id))
		FROM %[1]s.media_items mi LEFT JOIN %[1]s.metadata_items m ON m.id = mi.metadata_item_id`},
	{"tags", `SELECT concat_ws('|', tag_type, tag), coalesce(extra_data, '') FROM %[1]s.tags`},
	{"taggings", `SELECT concat_ws('|', m.guid, t.tag_type, t.tag, tg."index"), ''
		FROM %[1]s.taggings tg JOIN %[1]s.metadata_items m ON m.id = tg.metadata_item_id JOIN %[1]s.tags t ON t.id = tg.tag_id`},
}

// Compare reports the two schemas' row counts per table and the keyed
// differences, and whether anything differs.
func Compare(ctx context.Context, conn *pgx.Conn, a, b string, examples int) (string, bool, error) {
	var out strings.Builder
	differs := false
	tables, err := tablesOf(ctx, conn, a)
	if err != nil {
		return "", false, err
	}
	fmt.Fprintf(&out, "%-32s %12s %12s\n", "table", a, b)
	for _, t := range tables {
		ca, err := count(ctx, conn, a, t)
		if err != nil {
			return "", false, err
		}
		cb, err := count(ctx, conn, b, t)
		if err != nil {
			return "", false, err
		}
		mark := ""
		if ca != cb {
			mark, differs = "  <-- differs", true
		}
		fmt.Fprintf(&out, "%-32s %12d %12d%s\n", t, ca, cb, mark)
	}
	for _, k := range keyedTables {
		ra, err := rows(ctx, conn, k, a)
		if err != nil {
			return "", false, err
		}
		rb, err := rows(ctx, conn, k, b)
		if err != nil {
			return "", false, err
		}
		d := Diff(ra, rb)
		fmt.Fprintf(&out, "\n%s by key: %d only in %s, %d only in %s, %d differ\n", k.table, len(d.OnlyA), a, len(d.OnlyB), b, len(d.Changed))
		if len(d.OnlyA)+len(d.OnlyB)+len(d.Changed) > 0 {
			differs = true
		}
		for _, line := range d.Examples(ra, rb, examples) {
			fmt.Fprintln(&out, "  ", line)
		}
	}
	return out.String(), differs, nil
}

// Difference is what Diff found between two keyed row sets.
type Difference struct {
	OnlyA, OnlyB, Changed []string
}

// Diff compares two keyed row sets.
func Diff(a, b map[string]string) Difference {
	var d Difference
	for k, va := range a {
		vb, ok := b[k]
		switch {
		case !ok:
			d.OnlyA = append(d.OnlyA, k)
		case va != vb:
			d.Changed = append(d.Changed, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			d.OnlyB = append(d.OnlyB, k)
		}
	}
	sort.Strings(d.OnlyA)
	sort.Strings(d.OnlyB)
	sort.Strings(d.Changed)
	return d
}

// Examples renders up to n of each kind of difference.
func (d Difference) Examples(a, b map[string]string, n int) []string {
	var out []string
	for i, k := range d.OnlyA {
		if i >= n {
			break
		}
		out = append(out, "only in A: "+k+" = "+a[k])
	}
	for i, k := range d.OnlyB {
		if i >= n {
			break
		}
		out = append(out, "only in B: "+k+" = "+b[k])
	}
	for i, k := range d.Changed {
		if i >= n {
			break
		}
		out = append(out, "differs:   "+k+"\n       A: "+a[k]+"\n       B: "+b[k])
	}
	return out
}

func tablesOf(ctx context.Context, conn *pgx.Conn, schema string) ([]string, error) {
	rs, err := conn.Query(ctx, `SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'r' ORDER BY 1`, schema)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rs, pgx.RowTo[string])
}

func count(ctx context.Context, conn *pgx.Conn, schema, table string) (int64, error) {
	var n int64
	err := conn.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, table}.Sanitize()).Scan(&n)
	return n, err
}

func rows(ctx context.Context, conn *pgx.Conn, k keyed, schema string) (map[string]string, error) {
	rs, err := conn.Query(ctx, fmt.Sprintf(k.query, pgx.Identifier{schema}.Sanitize()))
	if err != nil {
		return nil, fmt.Errorf("%s of %s: %w", k.table, schema, err)
	}
	defer rs.Close()
	out := map[string]string{}
	for rs.Next() {
		var key, value string
		if err := rs.Scan(&key, &value); err != nil {
			return nil, err
		}
		if _, dup := out[key]; dup {
			out[key] += " (duplicate key)"
			continue
		}
		out[key] = value
	}
	return out, rs.Err()
}
