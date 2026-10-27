package plexseed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotInPlex is a file Plex has no part for in the seeder's libraries:
// not scanned yet, or in a library the seeder must not touch.
var ErrNotInPlex = errors.New("plexseed: file not in a seeded Plex library")

// Beginner opens a transaction: *pgxpool.Pool and *pgx.Conn both are.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Seeder writes a file's streams and markers into Plex's library.
type Seeder struct {
	DB Beginner
	// Libraries are the Plex library names the seeder may write into:
	// the ones cluster-plex provisioned for clustarr.
	Libraries []string
	Now       func() time.Time
}

// Result is what one Seed changed.
type Result struct {
	// Parts is how many Plex media items the file backs.
	Parts                                       int
	MediaSeeded, MarkersWritten, MarkersRemoved int
}

// Target is one media row the seeder writes to: its media_parts id,
// media_items id and metadata_items id.
type Target struct{ Part, Media, Item int64 }

// Seed writes in's probe into Plex for the file at plexPath when Plex has
// no streams for it, and reconciles its markers, in one transaction.
func (s *Seeder) Seed(ctx context.Context, plexPath string, in Input) (Result, error) {
	var res Result
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("plexseed: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
SELECT mp.id, mi.id, mi.metadata_item_id
  FROM media_parts mp
  JOIN media_items mi ON mi.id = mp.media_item_id
  JOIN library_sections ls ON ls.id = mi.library_section_id
 WHERE mp.file = $1 AND mp.deleted_at IS NULL AND mi.deleted_at IS NULL AND ls.name = ANY($2)`, plexPath, s.Libraries)
	if err != nil {
		return res, fmt.Errorf("plexseed: find %s: %w", plexPath, err)
	}
	targets, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Target, error) {
		var t Target
		return t, r.Scan(&t.Part, &t.Media, &t.Item)
	})
	if err != nil {
		return res, fmt.Errorf("plexseed: find %s: %w", plexPath, err)
	}
	if len(targets) == 0 {
		return res, fmt.Errorf("%w: %s", ErrNotInPlex, plexPath)
	}
	seen := map[int64]bool{}
	for _, t := range targets {
		r, err := s.seedTarget(ctx, tx, t, in, !seen[t.Item])
		if err != nil {
			return res, err
		}
		seen[t.Item] = true
		res.Parts += r.Parts
		res.MediaSeeded += r.MediaSeeded
		res.MarkersWritten += r.MarkersWritten
		res.MarkersRemoved += r.MarkersRemoved
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("plexseed: commit: %w", err)
	}
	return res, nil
}

// SeedTx seeds one target inside tx: its media row and streams, then its
// item's markers. Seed finds a path's targets and calls it in a transaction
// of its own; the projection (pkg/plexproject) calls it inside the
// transaction that wrote the row. A probe without a runtime (an unprobed
// file) seeds no media.
func (s *Seeder) SeedTx(ctx context.Context, tx pgx.Tx, t Target, in Input) (Result, error) {
	return s.seedTarget(ctx, tx, t, in, true)
}

// ReplaceTx is SeedTx for a row the projection owns outright: every stream
// of the target goes, Plex's own included, and the probe's are seeded in
// their place. A row adopted from Plex's scanner, or whose probe changed,
// takes this path; Plex never re-analyses a projected row, so nothing else
// would replace a stale stream (review C3, 2026-10-15).
func (s *Seeder) ReplaceTx(ctx context.Context, tx pgx.Tx, t Target, in Input) (Result, error) {
	if _, err := tx.Exec(ctx, "DELETE FROM media_streams WHERE media_item_id = $1", t.Media); err != nil {
		return Result{}, fmt.Errorf("plexseed: clear streams of media %d: %w", t.Media, err)
	}
	return s.seedTarget(ctx, tx, t, in, true)
}

func (s *Seeder) seedTarget(ctx context.Context, tx pgx.Tx, t Target, in Input, markers bool) (Result, error) {
	res := Result{Parts: 1}
	now := s.now().Unix()
	if in.Probe != nil && in.Probe.RuntimeMillis > 0 {
		seeded, err := s.seedMedia(ctx, tx, t, in, now)
		if err != nil {
			return res, err
		}
		if seeded {
			res.MediaSeeded++
		}
	}
	if !markers {
		return res, nil
	}
	var duration int64
	if in.Probe != nil {
		duration = in.Probe.RuntimeMillis
	}
	if want := MarkerRows(in.Markers, duration); want != nil {
		w, r, err := s.reconcileMarkers(ctx, tx, t.Item, want, now)
		if err != nil {
			return res, err
		}
		res.MarkersWritten, res.MarkersRemoved = w, r
	}
	return res, nil
}

// setAudioLanguages sets the language of the seeded audio streams the probe
// they were written from does not tag to the item's original language, in
// place: a viewer's chosen track names a stream by id. It is how a file
// seeded before the seeder knew the original language, or whose item's
// original language changed, catches up.
func (s *Seeder) setAudioLanguages(ctx context.Context, tx pgx.Tx, t Target, in Input, now int64) error {
	for _, a := range in.Probe.Audio {
		if PlexLanguage(a.Language) != "" {
			continue
		}
		if _, err := tx.Exec(ctx, `
UPDATE media_streams SET language = $3, updated_at = $5
 WHERE media_item_id = $1 AND stream_type_id = 2 AND "index" = $2 AND extra_data LIKE $4
   AND language IS DISTINCT FROM $3`,
			t.Media, a.Index, AudioLanguage(a, in.OriginalLanguage), "%"+ourMark+"%", now); err != nil {
			return fmt.Errorf("plexseed: audio language of media %d: %w", t.Media, err)
		}
	}
	return nil
}

// seedMedia fills an unanalysed media item from the probe, and refills one
// whose streams are all ours from another probe (a file replaced in
// place). One with any stream Plex analysed is left alone; one seeded from
// this probe has its untagged audio set to the original language.
func (s *Seeder) seedMedia(ctx context.Context, tx pgx.Tx, t Target, in Input, now int64) (bool, error) {
	var streams, ourStreams int
	var extra string
	if err := tx.QueryRow(ctx, `
SELECT (SELECT count(*) FROM media_streams WHERE media_item_id = $1),
       (SELECT count(*) FROM media_streams WHERE media_item_id = $1 AND extra_data LIKE $2),
       coalesce((SELECT extra_data FROM media_items WHERE id = $1), '')`,
		t.Media, "%"+ourMark+"%").Scan(&streams, &ourStreams, &extra); err != nil {
		return false, fmt.Errorf("plexseed: count streams of media %d: %w", t.Media, err)
	}
	switch {
	case streams == 0:
	case ourStreams == streams && in.ProbeHash != "" && !strings.Contains(extra, probeHashMark(in.ProbeHash)):
		if _, err := tx.Exec(ctx, "DELETE FROM media_streams WHERE media_item_id = $1 AND extra_data LIKE $2",
			t.Media, "%"+ourMark+"%"); err != nil {
			return false, fmt.Errorf("plexseed: clear streams of media %d: %w", t.Media, err)
		}
	case ourStreams == streams:
		return false, s.setAudioLanguages(ctx, tx, t, in, now)
	default:
		return false, nil
	}
	m, rows := MediaRows(*in.Probe, in.ProbeHash, in.OriginalLanguage)
	if in.FastStart != nil {
		v := int32(0)
		if *in.FastStart {
			v = 1
		}
		m.OptimizedForStreaming = &v
	}
	// optimized_for_streaming is written only when known: a NULL in $16 keeps
	// the column, Plex's own analysis included.
	if _, err := tx.Exec(ctx, `
UPDATE media_items SET container = $2, video_codec = $3, audio_codec = $4, width = $5, height = $6,
       duration = $7, bitrate = $8, frames_per_second = $9, display_aspect_ratio = $10,
       audio_channels = $11, extra_data = $12, updated_at = $13, color_trc = $14, media_analysis_version = $15,
       optimized_for_streaming = coalesce($16, optimized_for_streaming)
 WHERE id = $1`, t.Media, m.Container, m.VideoCodec, nullString(m.AudioCodec), m.Width, m.Height,
		m.Duration, m.Bitrate, m.FPS, m.AspectRatio, m.AudioChannels, m.ItemExtra, now, m.ColorTRC, m.AnalysisVersion,
		m.OptimizedForStreaming); err != nil {
		return false, fmt.Errorf("plexseed: media item %d: %w", t.Media, err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE media_parts SET duration = $2, size = CASE WHEN $3::bigint > 0 THEN $3::bigint ELSE size END,
       extra_data = $4, updated_at = $5
 WHERE id = $1`, t.Part, m.Duration, in.SizeBytes, m.PartExtra, now); err != nil {
		return false, fmt.Errorf("plexseed: media part %d: %w", t.Part, err)
	}
	// language is "" for none, never NULL: Plex's scanner stores "", and PMS
	// reads it without a NULL indicator (a NULL failed the show's page).
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `
INSERT INTO media_streams (stream_type_id, media_item_id, media_part_id, codec, language, "index",
                           channels, bitrate, "default", forced, extra_data, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`,
			r.Type, t.Media, t.Part, r.Codec, r.Language, r.Index,
			nullInt(r.Channels), nullInt(r.Bitrate), boolInt(r.Default), boolInt(r.Forced), r.Extra, now); err != nil {
			return false, fmt.Errorf("plexseed: stream of media %d: %w", t.Media, err)
		}
	}
	return true, nil
}

type markerRow struct {
	id         int64
	text       string
	index      int64
	start, end int64
	extra      string
}

// reconcileMarkers makes item's markers of each kind in want match it:
// a kind with rows is ours (Plex's detected rows of it go), a kind with
// none keeps Plex's rows and loses only ours.
func (s *Seeder) reconcileMarkers(ctx context.Context, tx pgx.Tx, item int64, want map[string][]MarkerRow, now int64) (written, removed int, err error) {
	rows, err := tx.Query(ctx, `
SELECT g.id, coalesce(g.text, ''), coalesce(g."index", 0), coalesce(g.time_offset, 0),
       coalesce(g.end_time_offset, 0), coalesce(g.extra_data, '')
  FROM taggings g JOIN tags t ON t.id = g.tag_id AND t.tag_type = 12
 WHERE g.metadata_item_id = $1`, item)
	if err != nil {
		return 0, 0, fmt.Errorf("plexseed: markers of %d: %w", item, err)
	}
	have, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (markerRow, error) {
		var m markerRow
		return m, r.Scan(&m.id, &m.text, &m.index, &m.start, &m.end, &m.extra)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("plexseed: markers of %d: %w", item, err)
	}

	var tag int64
	for kind, desired := range want {
		var ofKind, drop []markerRow
		for _, h := range have {
			if h.text == kind {
				ofKind = append(ofKind, h)
			}
		}
		switch {
		case len(desired) == 0:
			for _, h := range ofKind {
				if ours(h.extra) {
					drop = append(drop, h)
				}
			}
		case !same(ofKind, desired):
			drop = ofKind
		default:
			continue
		}
		for _, h := range drop {
			if _, err := tx.Exec(ctx, "DELETE FROM taggings WHERE id = $1", h.id); err != nil {
				return written, removed, fmt.Errorf("plexseed: delete marker %d: %w", h.id, err)
			}
			removed++
		}
		if len(desired) == 0 {
			continue
		}
		if tag == 0 {
			if tag, err = markerTag(ctx, tx, now); err != nil {
				return written, removed, err
			}
		}
		for _, d := range desired {
			if _, err := tx.Exec(ctx, `
INSERT INTO taggings (metadata_item_id, tag_id, "index", text, time_offset, end_time_offset, created_at, extra_data)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, item, tag, d.Index, d.Text, d.Start, d.End, now, markerExtra(d)); err != nil {
				return written, removed, fmt.Errorf("plexseed: insert marker on %d: %w", item, err)
			}
			written++
		}
	}
	return written, removed, nil
}

// same reports whether have is exactly desired, all rows ours.
func same(have []markerRow, desired []MarkerRow) bool {
	if len(have) != len(desired) {
		return false
	}
	for _, d := range desired {
		found := false
		for _, h := range have {
			if ours(h.extra) && h.index == int64(d.Index) && h.start == d.Start && h.end == d.End &&
				strings.Contains(h.extra, `"pv:final":"1"`) == d.Final {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ourMark is how a media row the seeder wrote reads in its extra_data.
const ourMark = `"` + SourceKey + `":"` + SourceValue + `"`

func probeHashMark(h string) string {
	b, _ := json.Marshal(h)
	return `"` + ProbeHashKey + `":` + string(b)
}

// ours reports whether a marker row is the seeder's: tagged with any source
// clustarr merges into status.markers.
func ours(extra string) bool {
	for _, src := range []string{MarkerSource, "chapters", "analysis"} {
		if strings.Contains(extra, `"`+MarkerKey+`":"`+src+`"`) {
			return true
		}
	}
	return false
}

// markerTag is the library's marker tag (tag_type 12, empty name), created
// as Plex's is when the library has none yet.
func markerTag(ctx context.Context, tx pgx.Tx, now int64) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, "SELECT id FROM tags WHERE tag_type = 12 ORDER BY id LIMIT 1").Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("plexseed: marker tag: %w", err)
	}
	if err := tx.QueryRow(ctx,
		"INSERT INTO tags (tag, tag_type, created_at, updated_at) VALUES ('', 12, $1, $1) RETURNING id", now,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("plexseed: create marker tag: %w", err)
	}
	return id, nil
}

func (s *Seeder) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(v int32) any {
	if v == 0 {
		return nil
	}
	return v
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
