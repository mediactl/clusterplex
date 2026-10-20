package plexseed

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Plex finds a subtitle file beside a media file only while it scans, and
// for the projection's sections its scanner no longer runs, so the
// projection writes them as Plex's scanner does. The layout below is
// captured from Plex 1.43.4's own scanner on a folder of sidecars
// (2026-10-20): a media_streams row per file, stream_type_id 3, url the
// file:// URL of the file, codec its format, language the tag in its name
// as written ("en", "pt-BR", "zh-Hant"), forced in its column,
// hearing-impaired as ma:hearingImpaired in extra_data, ma:format the
// codec; no index, url_index, channels or bitrate, default 0.

// Sidecar is a subtitle file beside a media file: an entry of clustarr's
// MediaFile status.sidecars.
type Sidecar struct {
	// Name is the file's name in the media file's directory.
	Name     string `json:"name,omitempty"`
	Language string `json:"language,omitempty"`
	Forced   bool   `json:"forced,omitempty"`
	HI       bool   `json:"hi,omitempty"`
}

// SidecarRow is the media_streams row of a sidecar.
type SidecarRow struct {
	URL, Codec, Language string
	Forced               bool
	Extra                string
}

// subtitleFormats are the sidecar formats Plex reads as subtitles.
var subtitleFormats = map[string]bool{"srt": true, "ass": true, "ssa": true, "vtt": true, "smi": true}

// SidecarURL is the url Plex's scanner stores for a file at path: file://
// and the path with only '%' and ' ' escaped; every other byte, a tab
// included, is written as it is.
func SidecarURL(path string) string {
	return "file://" + strings.NewReplacer("%", "%25", " ", "%20").Replace(path)
}

// SidecarRows are the rows of the sidecars of the media file at plexPath,
// Plex's path for it. A sidecar of a format Plex does not read as a
// subtitle, or whose name is not a bare file name, has none.
func SidecarRows(plexPath string, sidecars []Sidecar) []SidecarRow {
	dir := path.Dir(plexPath)
	var out []SidecarRow
	for _, s := range sidecars {
		if s.Name == "" || strings.ContainsRune(s.Name, '/') || s.Name == "." || s.Name == ".." {
			continue
		}
		format := strings.ToLower(strings.TrimPrefix(path.Ext(s.Name), "."))
		if !subtitleFormats[format] {
			continue
		}
		kv := map[string]string{SourceKey: SourceValue, "ma:format": format}
		if s.HI {
			kv["ma:hearingImpaired"] = "1"
		}
		out = append(out, SidecarRow{
			URL: SidecarURL(dir + "/" + s.Name), Codec: format, Language: s.Language,
			Forced: s.Forced, Extra: ExtraData(kv),
		})
	}
	return out
}

// SyncSidecarsTx makes media t's external subtitle streams the sidecars of
// the file at plexPath, in place: a row whose sidecar is gone is deleted,
// a new one inserted, and one still there keeps its stream id -- a
// viewer's chosen subtitle names its stream by id -- with its codec,
// language, forced flag and extra_data brought up to date. Embedded
// streams are not touched. The caller owns the row's streams (the
// projection, pkg/plexproject).
func (s *Seeder) SyncSidecarsTx(ctx context.Context, tx pgx.Tx, t Target, plexPath string, sidecars []Sidecar) (written, removed int, err error) {
	want := map[string]SidecarRow{}
	var order []string
	for _, r := range SidecarRows(plexPath, sidecars) {
		if _, dup := want[r.URL]; !dup {
			order = append(order, r.URL)
		}
		want[r.URL] = r
	}
	rows, err := tx.Query(ctx, `SELECT id, url, coalesce(codec, ''), coalesce(language, ''), coalesce(forced, 0), coalesce(extra_data, '')
		FROM media_streams WHERE media_item_id = $1 AND stream_type_id = 3 AND url LIKE 'file://%' ORDER BY id`, t.Media)
	if err != nil {
		return 0, 0, fmt.Errorf("plexseed: sidecars of media %d: %w", t.Media, err)
	}
	type have struct {
		id      int64
		current bool
	}
	haves := map[string][]have{}
	for rows.Next() {
		var id int64
		var url, codec, language, extra string
		var forced int
		if err := rows.Scan(&id, &url, &codec, &language, &forced, &extra); err != nil {
			rows.Close()
			return 0, 0, err
		}
		w, ok := want[url]
		haves[url] = append(haves[url], have{id, ok && w.Codec == codec && w.Language == language && boolInt(w.Forced) == forced && w.Extra == extra})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	now := s.now().Unix()
	for url, hs := range haves {
		for i, h := range hs {
			w, wanted := want[url]
			switch {
			case !wanted || i > 0:
				// Gone, or a second row for one file.
				if _, err := tx.Exec(ctx, `DELETE FROM media_streams WHERE id = $1`, h.id); err != nil {
					return written, removed, fmt.Errorf("plexseed: remove sidecar of media %d: %w", t.Media, err)
				}
				removed++
			case !h.current:
				if _, err := tx.Exec(ctx, `UPDATE media_streams SET codec = $2, language = $3, forced = $4, extra_data = $5, media_part_id = $6, updated_at = $7 WHERE id = $1`,
					h.id, w.Codec, w.Language, boolInt(w.Forced), w.Extra, t.Part, now); err != nil {
					return written, removed, fmt.Errorf("plexseed: update sidecar of media %d: %w", t.Media, err)
				}
				written++
			}
		}
	}
	for _, url := range order {
		if _, ok := haves[url]; ok {
			continue
		}
		w := want[url]
		if _, err := tx.Exec(ctx, `
INSERT INTO media_streams (stream_type_id, media_item_id, media_part_id, url, codec, language, "default", forced, extra_data, created_at, updated_at)
VALUES (3, $1, $2, $3, $4, $5, 0, $6, $7, $8, $8)`, t.Media, t.Part, w.URL, w.Codec, w.Language, boolInt(w.Forced), w.Extra, now); err != nil {
			return written, removed, fmt.Errorf("plexseed: add sidecar of media %d: %w", t.Media, err)
		}
		written++
	}
	return written, removed, nil
}
