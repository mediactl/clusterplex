package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mediactl/clusterplex/pkg/plex/extradata"
)

// builtComment marks a guide schema the managers built (pkg/plex/guidebuild),
// not one copied from a load PMS made: a cloud guide built from Plex's guide
// service; builtXMLTVComment an XMLTV DVR's built from clustarr's XMLTV.
// Either reads back as the build's time (guideBuiltAt).
const (
	builtComment      = "clusterplex: built from Plex's guide service at %d (Unix ns)"
	builtXMLTVComment = "clusterplex: built from clustarr's XMLTV at %d (Unix ns)"
)

// builtComments are the comments of a guide the managers built.
var builtComments = []string{builtComment, builtXMLTVComment}

// builtCommentFor is the comment a build of g's guide gets.
func builtCommentFor(g Guide) string {
	if g.Provider == xmltvProvider {
		return builtXMLTVComment
	}
	return builtComment
}

// BuildFunc writes a guide's rows into staging in tx: everything but the
// sections, playlists and viewing rows InstallBuilt carries over.
type BuildFunc func(ctx context.Context, tx pgx.Tx, staging string) error

// InstallBuilt makes g's guide what build writes, the way ImportGuide makes
// it a load's copy: StageBuilt, then SwapStaged, on one connection. A build
// that fails leaves the old guide. The lease holder runs this when the
// guide is built in its own process (plex.liveTV.cloudGuideBuild
// "inProcess"); a build Job runs StageBuilt and the holder SwapStaged.
// docs/superpowers/specs/2026-11-10-guide-writer-and-built-cloud-guide-design.md, plan B.
func InstallBuilt(ctx context.Context, log *slog.Logger, conn *pgx.Conn, library string, g Guide, build BuildFunc) error {
	stamp := fmt.Sprintf("in-process-%d", time.Now().UnixNano())
	if err := StageBuilt(ctx, log, conn, library, g, stamp, build); err != nil {
		return err
	}
	return SwapStaged(ctx, log, conn, library, g, stamp)
}

// buildsTable records each guide whose staging schema holds a finished
// build that SwapStaged has yet to install: one row per guide schema,
// written in the build's own transaction, so a row never names a staging
// schema whose build did not finish, and deleted by the swap.
const buildsTable = "clusterplex.guide_builds"

// ErrNotStaged is SwapStaged's answer when g's staging schema does not hold
// the build it names: another build replaced it, or none finished.
var ErrNotStaged = errors.New("the staging schema does not hold that build")

// ensureBuilds makes buildsTable.
func ensureBuilds(ctx context.Context, conn *pgx.Conn) error {
	for _, s := range []string{
		"CREATE SCHEMA IF NOT EXISTS clusterplex",
		"CREATE TABLE IF NOT EXISTS " + buildsTable + " (schema text PRIMARY KEY, stamp text NOT NULL, staged_at timestamptz NOT NULL)",
	} {
		if _, err := conn.Exec(ctx, s); err != nil {
			return fmt.Errorf("make %s: %w", buildsTable, err)
		}
	}
	return nil
}

// lockGuide takes g's advisory lock on conn, the one ImportGuide, StageLoad
// and the swaps take, and returns its release. A session lock: a connection
// that drops (a build Job's pod killed) releases it.
func lockGuide(ctx context.Context, conn *pgx.Conn, g Guide) (func(), error) {
	key := "clusterplex-guide-" + g.Schema()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", key); err != nil {
		return nil, err
	}
	return func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext($1))", key) }, nil
}

// StageBuilt writes build's rows into g's staging schema, cloned clean from
// the library's, with the live guide's sections (PMS's three for a guide
// that has none yet: a DVR whose first load never landed), and records the
// build under stamp in buildsTable in the same transaction. The live guide
// is not touched; SwapStaged installs the build. A build that fails drops
// the staging schema.
func StageBuilt(ctx context.Context, log *slog.Logger, conn *pgx.Conn, library string, g Guide, stamp string, build BuildFunc) error {
	if !g.Valid() {
		return fmt.Errorf("not a guide the shim reads: %+v", g)
	}
	if stamp == "" {
		return errors.New("a staged build needs a stamp")
	}
	if err := ensureBuilds(ctx, conn); err != nil {
		return err
	}
	unlock, err := lockGuide(ctx, conn, g)
	if err != nil {
		return err
	}
	defer unlock()
	schema, staging := g.Schema(), g.Staging()
	// The record goes first: a staging schema this build is about to
	// replace must never read as another build's.
	if _, err := conn.Exec(ctx, "DELETE FROM "+buildsTable+" WHERE schema = $1", schema); err != nil {
		return err
	}
	// Never a staging schema a load left: a build starts from empty tables.
	if err := dropSchema(ctx, conn, staging); err != nil {
		return fmt.Errorf("drop %s: %w", staging, err)
	}
	start := time.Now()
	if err := CloneSchema(ctx, conn, library, staging); err != nil {
		return fmt.Errorf("make %s from %s: %w", staging, library, err)
	}
	live, err := schemaExists(ctx, conn, schema)
	if err == nil {
		err = fillBuilt(ctx, conn, g, staging, live, stamp, build)
	}
	if err != nil {
		if dropErr := dropSchema(context.Background(), conn, staging); dropErr != nil {
			log.Warn("drop a guide's staging schema", "schema", staging, "error", dropErr)
		}
		return fmt.Errorf("build %s: %w", schema, err)
	}
	if aerr := analyzeSchema(ctx, conn, staging); aerr != nil {
		log.Warn("analyze a guide's staging schema", "schema", staging, "error", aerr)
	}
	log.Info("staged a Live TV guide built from "+builtFrom(g), "guide", g.File(), "schema", staging,
		"stamp", stamp, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// StagedBuild is the stamp of the finished build g's staging schema holds
// and when it was staged; "" when it holds none.
func StagedBuild(ctx context.Context, conn *pgx.Conn, g Guide) (stamp string, at time.Time, err error) {
	if err := ensureBuilds(ctx, conn); err != nil {
		return "", time.Time{}, err
	}
	err = conn.QueryRow(ctx, "SELECT stamp, staged_at FROM "+buildsTable+" WHERE schema = $1", g.Schema()).Scan(&stamp, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, nil
	}
	return stamp, at, err
}

// SwapStaged installs the build StageBuilt staged for g under stamp: the
// live guide's playlists (renumbered past the build's items, as PMS numbers
// them past a load's) and its viewing rows carried over at this moment,
// then the schemas renamed, so PMS reads the old guide or the new one. It
// answers ErrNotStaged, touching nothing, when the staging schema does not
// hold that build.
func SwapStaged(ctx context.Context, log *slog.Logger, conn *pgx.Conn, library string, g Guide, stamp string) error {
	if !g.Valid() {
		return fmt.Errorf("not a guide the shim reads: %+v", g)
	}
	if err := ensureBuilds(ctx, conn); err != nil {
		return err
	}
	unlock, err := lockGuide(ctx, conn, g)
	if err != nil {
		return err
	}
	defer unlock()
	schema, staging, old := g.Schema(), g.Staging(), g.Replaced()
	have, _, err := StagedBuild(ctx, conn, g)
	if err != nil {
		return err
	}
	if have != stamp {
		return fmt.Errorf("%s: %w (it holds %q, not %q)", staging, ErrNotStaged, have, stamp)
	}
	if exists, err := schemaExists(ctx, conn, staging); err != nil {
		return err
	} else if !exists {
		_, _ = conn.Exec(ctx, "DELETE FROM "+buildsTable+" WHERE schema = $1", schema)
		return fmt.Errorf("%s: %w (the schema is gone)", staging, ErrNotStaged)
	}
	if err := dropSchema(ctx, conn, old); err != nil {
		log.Debug("a replaced guide is still read; it is dropped later", "schema", old, "error", err)
	}
	live, err := schemaExists(ctx, conn, schema)
	if err == nil && live {
		err = carryLive(ctx, conn, schema, staging)
	}
	if err == nil {
		err = swapGuide(ctx, conn, schema, staging, old, fmt.Sprintf(builtCommentFor(g), time.Now().UnixNano()))
	}
	if err != nil {
		return fmt.Errorf("install %s: %w", schema, err)
	}
	if _, err := conn.Exec(ctx, "DELETE FROM "+buildsTable+" WHERE schema = $1", schema); err != nil {
		log.Warn("clear an installed build's record", "schema", schema, "error", err)
	}
	if err := dropSchema(ctx, conn, old); err != nil {
		log.Debug("a replaced guide is still read; it is dropped later", "schema", old, "error", err)
	}
	log.Info("built a Live TV guide from "+builtFrom(g), "guide", g.File(), "schema", schema, "stamp", stamp)
	return nil
}

// builtFrom is what g's guide is built from, for the log.
func builtFrom(g Guide) string {
	if g.Provider == xmltvProvider {
		return "its XMLTV"
	}
	return "Plex's guide service"
}

// fillBuilt writes staging's sections and build's rows, and records the
// build under stamp, in one transaction.
func fillBuilt(ctx context.Context, conn *pgx.Conn, g Guide, staging string, live bool, stamp string, build BuildFunc) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	schema := g.Schema()
	s, l := pgx.Identifier{staging}.Sanitize(), pgx.Identifier{schema}.Sanitize()
	var sections int
	if live {
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+l+".library_sections").Scan(&sections); err != nil {
			return err
		}
	}
	if sections > 0 {
		if _, err := tx.Exec(ctx, "INSERT INTO "+s+".library_sections SELECT * FROM "+l+".library_sections"); err != nil {
			return fmt.Errorf("carry the sections over: %w", err)
		}
	}
	if err := seedSections(ctx, tx, staging, g.Provider); err != nil {
		return err
	}
	if err := build(ctx, tx, staging); err != nil {
		return err
	}
	if err := resetSequences(ctx, tx, staging); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO "+buildsTable+" (schema, stamp, staged_at) VALUES ($1, $2, now()) "+
		"ON CONFLICT (schema) DO UPDATE SET stamp = EXCLUDED.stamp, staged_at = EXCLUDED.staged_at", schema, stamp); err != nil {
		return fmt.Errorf("record the build: %w", err)
	}
	return tx.Commit(ctx)
}

// carryLive copies the live guide's playlists and viewing rows into
// staging, in one transaction, as the swap takes it: what a person did in
// the guide while the build ran is carried too.
func carryLive(ctx context.Context, conn *pgx.Conn, schema, staging string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	s, l := pgx.Identifier{staging}.Sanitize(), pgx.Identifier{schema}.Sanitize()
	if err := carryPlaylists(ctx, tx, schema, staging); err != nil {
		return err
	}
	for _, t := range []string{"metadata_item_settings", "metadata_item_views"} {
		if _, err := tx.Exec(ctx, "INSERT INTO "+s+"."+t+" SELECT * FROM "+l+"."+t); err != nil {
			return fmt.Errorf("carry %s over: %w", t, err)
		}
	}
	if err := resetSequences(ctx, tx, staging); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// xmltvProvider is an XMLTV DVR's guide provider (guidebuild.ProviderXMLTV,
// which imports this package).
const xmltvProvider = "tv.plex.providers.epg.xmltv"

// seedSections makes each section PMS's load of a provider's guide makes
// that staging lacks by name, under PMS's id for it when that is free and
// past the highest otherwise: a cloud load's Movies, then Shows and Sports
// with seasons flattened, as PMS wrote them on the rig (2026-11-10); an
// XMLTV load's News besides, its fourth (custom guides design §2.3), seeded
// as Shows and Sports are. The sections a live guide holds are copied
// before, so this makes all of them only for a guide whose first load
// never landed.
func seedSections(ctx context.Context, tx pgx.Tx, staging, provider string) error {
	now := time.Now().Unix()
	flatten := extradata.Encode(map[string]string{"pr:flattenSeasons": "1"})
	type section struct {
		name   string
		typ    int
		fields string
	}
	sections := []section{{"Movies", 1, ""}, {"Shows", 2, flatten}, {"Sports", 2, flatten}}
	if provider == xmltvProvider {
		sections = append(sections, section{"News", 2, flatten})
	}
	table := pgx.Identifier{staging, "library_sections"}.Sanitize()
	for i, s := range sections {
		var have bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+table+" WHERE name = $1)", s.name).Scan(&have); err != nil {
			return err
		}
		if have {
			continue
		}
		if _, err := tx.Exec(ctx, "INSERT INTO "+table+
			` (id, name, section_type, created_at, updated_at, user_fields, uuid, changed_at, content_changed_at)
			VALUES (CASE WHEN EXISTS (SELECT 1 FROM `+table+` WHERE id = $1) THEN (SELECT max(id) + 1 FROM `+table+`) ELSE $1 END,
				$2, $3, $4, $4, $5, gen_random_uuid()::text, 0, 0)`, i+1, s.name, s.typ, now, s.fields); err != nil {
			return fmt.Errorf("make the %s section: %w", s.name, err)
		}
	}
	return nil
}

// carryPlaylists copies the live guide's playlists (metadata type 15) past
// the build's items, and the accounts' rows that point at them.
func carryPlaylists(ctx context.Context, tx pgx.Tx, schema, staging string) error {
	rows, err := tx.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'metadata_items' AND column_name <> 'id' ORDER BY ordinal_position`, staging)
	if err != nil {
		return err
	}
	cols, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = pgx.Identifier{c}.Sanitize()
	}
	list := strings.Join(quoted, ", ")
	s, l := pgx.Identifier{staging}.Sanitize(), pgx.Identifier{schema}.Sanitize()
	q := fmt.Sprintf(`WITH base AS (SELECT coalesce(max(id), 0) AS b FROM %[1]s.metadata_items),
		old AS (SELECT id, row_number() OVER (ORDER BY id) AS rn FROM %[2]s.metadata_items WHERE metadata_type = 15),
		moved AS (INSERT INTO %[1]s.metadata_items (id, %[3]s)
			SELECT base.b + old.rn, m.%[4]s FROM %[2]s.metadata_items m JOIN old ON old.id = m.id CROSS JOIN base
			RETURNING id)
		INSERT INTO %[1]s.metadata_item_accounts (account_id, metadata_item_id)
			SELECT a.account_id, base.b + old.rn FROM %[2]s.metadata_item_accounts a JOIN old ON old.id = a.metadata_item_id CROSS JOIN base`,
		s, l, list, strings.Join(quoted, ", m."))
	if _, err := tx.Exec(ctx, q); err != nil {
		return fmt.Errorf("carry the playlists over: %w", err)
	}
	return nil
}

// MarkGuideRefreshed writes a DVR's cloud guide provider row as PMS does at
// a refresh's end -- pv:timeOfLastRefresh refreshed, pv:lastChunkEndedAt
// the day after the last one built, and the row's updated_at refreshed too,
// which PMS reports as the DVR's refreshedAt (PMS's own refresh on the rig,
// 2026-11-10, wrote updated_at equal to pv:timeOfLastRefresh) -- keeping
// its other fields, so PMS's start-up check finds the guide fresh.
func MarkGuideRefreshed(ctx context.Context, conn *pgx.Conn, library, dvrUUID string, lastChunkEnded, refreshed time.Time) error {
	lib := pgx.Identifier{library}.Sanitize()
	var id int64
	var extra *string
	err := conn.QueryRow(ctx, `SELECT p.id, p.extra_data FROM `+lib+`.media_provider_resources p
		JOIN `+lib+`.media_provider_resources d ON d.id = p.parent_id
		WHERE d.identifier = 'tv.plex.harvesters.dvr' AND lower(d.uuid) = lower($1) AND p.type = 3`, dvrUUID).Scan(&id, &extra)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("DVR %s has no guide provider row", dvrUUID)
	} else if err != nil {
		return err
	}
	fields := map[string]string{}
	if extra != nil {
		if fields, err = extradata.Decode(*extra); err != nil {
			return fmt.Errorf("read the guide provider row: %w", err)
		}
	}
	fields["pv:lastChunkEndedAt"] = strconv.FormatInt(lastChunkEnded.Unix(), 10)
	fields["pv:timeOfLastRefresh"] = strconv.FormatInt(refreshed.Unix(), 10)
	_, err = conn.Exec(ctx, `UPDATE `+lib+`.media_provider_resources SET extra_data = $1, updated_at = $3 WHERE id = $2`,
		extradata.Encode(fields), id, refreshed.Unix())
	return err
}

// guideBuiltAt reads when a guide was last built, 0 if never: its comment
// is one of builtComments.
func guideBuiltAt(comment string) int64 {
	var ns int64
	for _, format := range builtComments {
		if _, err := fmt.Sscanf(comment, format, &ns); err == nil {
			return ns
		}
	}
	return 0
}

// BuiltGuideSchemas is the schemas of the live guides the managers built
// (their comment is one of builtComments), whatever their provider: a Plex cloud
// lineup's or an XMLTV DVR's. A staging or a replaced schema is none.
func BuiltGuideSchemas(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	rows, err := conn.Query(ctx, `SELECT nspname::text, obj_description(oid, 'pg_namespace') FROM pg_namespace
		WHERE nspname LIKE 'epg\_%' AND nspname NOT LIKE '%\_next' AND nspname NOT LIKE '%\_old' ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	type schema struct {
		name    string
		comment *string
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (schema, error) {
		var s schema
		err := r.Scan(&s.name, &s.comment)
		return s, err
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range all {
		if s.comment != nil && guideBuiltAt(*s.comment) > 0 {
			out = append(out, s.name)
		}
	}
	return out, nil
}

// GuideBuiltAt is when g's guide was last built, from Plex's guide service
// or clustarr's XMLTV, zero if it never was or a load has replaced it since.
func GuideBuiltAt(ctx context.Context, conn *pgx.Conn, g Guide) (time.Time, error) {
	var comment *string
	err := conn.QueryRow(ctx, `SELECT obj_description(n.oid, 'pg_namespace') FROM pg_namespace n WHERE n.nspname = $1`, g.Schema()).Scan(&comment)
	if errors.Is(err, pgx.ErrNoRows) || comment == nil {
		return time.Time{}, nil
	} else if err != nil {
		return time.Time{}, err
	}
	if ns := guideBuiltAt(*comment); ns > 0 {
		return time.Unix(0, ns), nil
	}
	return time.Time{}, nil
}
