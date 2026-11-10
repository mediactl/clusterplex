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

// builtComment marks a guide schema the managers built from Plex's guide
// service (pkg/plex/guidebuild), not one copied from a load PMS made.
const builtComment = "clusterplex: built from Plex's guide service at %d (Unix ns)"

// BuildFunc writes a guide's rows into staging in tx: everything but the
// sections, playlists and viewing rows InstallBuilt carries over.
type BuildFunc func(ctx context.Context, tx pgx.Tx, staging string) error

// InstallBuilt makes g's guide what build writes, the way ImportGuide makes
// it a load's copy: a staging schema cloned clean from the library's, the
// live guide's sections, its playlists (renumbered past the build's items,
// as PMS numbers them past a load's) and its viewing rows carried over,
// build's rows, then the schemas renamed, so PMS reads the old guide or the
// new one and a build that fails leaves the old one. A guide with no
// sections yet (a DVR whose first load never landed) gets PMS's three.
// docs/superpowers/specs/2026-11-10-guide-writer-and-built-cloud-guide-design.md, plan B.
func InstallBuilt(ctx context.Context, log *slog.Logger, conn *pgx.Conn, library string, g Guide, build BuildFunc) error {
	if !g.Valid() {
		return fmt.Errorf("not a guide the shim reads: %+v", g)
	}
	schema, staging, old := g.Schema(), g.Staging(), g.Replaced()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", "clusterplex-guide-"+schema); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext($1))", "clusterplex-guide-"+schema)
	}()
	if err := dropSchema(ctx, conn, old); err != nil {
		log.Debug("a replaced guide is still read; it is dropped later", "schema", old, "error", err)
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
		err = fillBuilt(ctx, conn, schema, staging, live, build)
	}
	if err == nil {
		if aerr := analyzeSchema(ctx, conn, staging); aerr != nil {
			log.Warn("analyze a guide's staging schema", "schema", staging, "error", aerr)
		}
		err = swapGuide(ctx, conn, schema, staging, old, fmt.Sprintf(builtComment, time.Now().UnixNano()))
	}
	if err != nil {
		if dropErr := dropSchema(context.Background(), conn, staging); dropErr != nil {
			log.Warn("drop a guide's staging schema", "schema", staging, "error", dropErr)
		}
		return fmt.Errorf("build %s: %w", schema, err)
	}
	if err := dropSchema(ctx, conn, old); err != nil {
		log.Debug("a replaced guide is still read; it is dropped later", "schema", old, "error", err)
	}
	log.Info("built a Live TV guide from Plex's guide service", "guide", g.File(), "schema", schema,
		"took", time.Since(start).Round(time.Millisecond))
	return nil
}

// fillBuilt writes staging's rows in one transaction.
func fillBuilt(ctx context.Context, conn *pgx.Conn, schema, staging string, live bool, build BuildFunc) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
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
	} else if err := seedSections(ctx, tx, staging); err != nil {
		return err
	}
	if err := build(ctx, tx, staging); err != nil {
		return err
	}
	if live {
		if err := carryPlaylists(ctx, tx, schema, staging); err != nil {
			return err
		}
		for _, t := range []string{"metadata_item_settings", "metadata_item_views"} {
			if _, err := tx.Exec(ctx, "INSERT INTO "+s+"."+t+" SELECT * FROM "+l+"."+t); err != nil {
				return fmt.Errorf("carry %s over: %w", t, err)
			}
		}
	}
	if err := resetSequences(ctx, tx, staging); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// seedSections writes the three sections PMS's cloud load makes, as it
// wrote them on the rig (2026-11-10): Movies, then Shows and Sports with
// seasons flattened.
func seedSections(ctx context.Context, tx pgx.Tx, staging string) error {
	now := time.Now().Unix()
	flatten := extradata.Encode(map[string]string{"pr:flattenSeasons": "1"})
	for i, s := range []struct {
		name   string
		typ    int
		fields string
	}{{"Movies", 1, ""}, {"Shows", 2, flatten}, {"Sports", 2, flatten}} {
		if _, err := tx.Exec(ctx, "INSERT INTO "+pgx.Identifier{staging, "library_sections"}.Sanitize()+
			` (id, name, section_type, created_at, updated_at, user_fields, uuid, changed_at, content_changed_at)
			VALUES ($1, $2, $3, $4, $4, $5, gen_random_uuid()::text, 0, 0)`, i+1, s.name, s.typ, now, s.fields); err != nil {
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

// guideBuiltAt reads when a guide was last built, 0 if never.
func guideBuiltAt(comment string) int64 {
	var ns int64
	if _, err := fmt.Sscanf(comment, builtComment, &ns); err != nil {
		return 0
	}
	return ns
}

// GuideBuiltAt is when g's guide was last built from Plex's guide service,
// zero if it never was or a load has replaced it since.
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
