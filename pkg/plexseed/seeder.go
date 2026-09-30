package plexseed

import (
	"context"
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

type target struct{ part, media, item int64 }

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
	targets, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (target, error) {
		var t target
		return t, r.Scan(&t.part, &t.media, &t.item)
	})
	if err != nil {
		return res, fmt.Errorf("plexseed: find %s: %w", plexPath, err)
	}
	if len(targets) == 0 {
		return res, fmt.Errorf("%w: %s", ErrNotInPlex, plexPath)
	}
	res.Parts = len(targets)
	now := s.now().Unix()

	for _, t := range targets {
		if in.Probe == nil || in.Probe.RuntimeMillis <= 0 {
			continue
		}
		seeded, err := s.seedMedia(ctx, tx, t, in, now)
		if err != nil {
			return res, err
		}
		if seeded {
			res.MediaSeeded++
		}
	}

	var duration int64
	if in.Probe != nil {
		duration = in.Probe.RuntimeMillis
	}
	if want := MarkerRows(in.Markers, duration); want != nil {
		seen := map[int64]bool{}
		for _, t := range targets {
			if seen[t.item] {
				continue
			}
			seen[t.item] = true
			w, r, err := s.reconcileMarkers(ctx, tx, t.item, want, now)
			if err != nil {
				return res, err
			}
			res.MarkersWritten += w
			res.MarkersRemoved += r
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("plexseed: commit: %w", err)
	}
	return res, nil
}

// seedMedia fills an unanalysed media item from the probe; one with any
// stream -- Plex's analysis, or ours -- is left alone.
func (s *Seeder) seedMedia(ctx context.Context, tx pgx.Tx, t target, in Input, now int64) (bool, error) {
	var streams int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM media_streams WHERE media_item_id = $1", t.media).Scan(&streams); err != nil {
		return false, fmt.Errorf("plexseed: count streams of media %d: %w", t.media, err)
	}
	if streams > 0 {
		return false, nil
	}
	m, rows := MediaRows(*in.Probe)
	if _, err := tx.Exec(ctx, `
UPDATE media_items SET container = $2, video_codec = $3, audio_codec = $4, width = $5, height = $6,
       duration = $7, bitrate = $8, frames_per_second = $9, display_aspect_ratio = $10,
       audio_channels = $11, extra_data = $12, updated_at = $13
 WHERE id = $1`, t.media, m.Container, m.VideoCodec, nullString(m.AudioCodec), m.Width, m.Height,
		m.Duration, m.Bitrate, m.FPS, m.AspectRatio, m.AudioChannels, m.ItemExtra, now); err != nil {
		return false, fmt.Errorf("plexseed: media item %d: %w", t.media, err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE media_parts SET duration = $2, size = CASE WHEN $3::bigint > 0 THEN $3::bigint ELSE size END,
       extra_data = $4, updated_at = $5
 WHERE id = $1`, t.part, m.Duration, in.SizeBytes, m.PartExtra, now); err != nil {
		return false, fmt.Errorf("plexseed: media part %d: %w", t.part, err)
	}
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `
INSERT INTO media_streams (stream_type_id, media_item_id, media_part_id, codec, language, "index",
                           channels, bitrate, "default", forced, extra_data, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`,
			r.Type, t.media, t.part, r.Codec, nullString(r.Language), r.Index,
			nullInt(r.Channels), nullInt(r.Bitrate), boolInt(r.Default), boolInt(r.Forced), r.Extra, now); err != nil {
			return false, fmt.Errorf("plexseed: stream of media %d: %w", t.media, err)
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

func ours(extra string) bool { return strings.Contains(extra, `"`+MarkerKey+`":"`+MarkerSource+`"`) }

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
