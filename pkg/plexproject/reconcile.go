package plexproject

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// Outcome counts what a reconcile did.
type Outcome struct {
	Applied, Deleted, Adopted, Unknown, Failed int
}

// rowState is one live media row of the section: its stamps, if any, and
// its file.
type rowState struct {
	uid, seedKey, file string
}

// Reconcile is spec §8 for a section (folder == "") or one folder of it.
// The database side is every live media row there; the clustarr side is
// want, every MediaFile there. A row whose UID names no MediaFile is
// deleted; a MediaFile with no row, or a drifted seed key, is applied; a
// stamp-less row at a path a MediaFile names is adopted by that apply; a
// stamp-less row nobody names is a file Plex scanned that clustarr does
// not know, counted and left. A MediaFile whose item the caller could not
// resolve yet is wanted but not applied, so its row stays.
func (p *Projector) Reconcile(ctx context.Context, section, location int64, folder string, want []Change) (Outcome, error) {
	var out Outcome
	have, byPath, err := p.liveRows(ctx, section, folder)
	if err != nil {
		return out, err
	}
	wanted := map[string]bool{}
	for _, c := range want {
		if !underFolder(c.PlexPath, folder) {
			continue
		}
		wanted[c.UID] = true
		if c.Unresolved {
			continue
		}
		st, ok := have[c.UID]
		if ok && st.seedKey == c.SeedKey {
			continue
		}
		if !ok {
			if _, adopt := byPath[c.PlexPath]; adopt {
				out.Adopted++
				delete(byPath, c.PlexPath)
			}
		}
		if err := p.Apply(ctx, section, location, c); err != nil {
			out.Failed++
			p.log().WarnContext(ctx, "plexproject: apply during reconcile", "file", c.UID, "path", c.PlexPath, "error", err)
			continue
		}
		out.Applied++
	}
	for uid := range have {
		if wanted[uid] {
			continue
		}
		if err := p.Delete(ctx, section, uid); err != nil {
			out.Failed++
			p.log().WarnContext(ctx, "plexproject: delete during reconcile", "file", uid, "error", err)
			continue
		}
		out.Deleted++
	}
	out.Unknown = len(byPath)
	return out, nil
}

// liveRows reads the section's (or the folder's) live media rows: the
// stamped ones by UID, the stamp-less ones by file.
func (p *Projector) liveRows(ctx context.Context, section int64, folder string) (map[string]rowState, map[string]rowState, error) {
	rows, err := p.DB.Query(ctx, `SELECT mi.extra_data, mp.file FROM media_items mi JOIN media_parts mp ON mp.media_item_id = mi.id
		WHERE mi.library_section_id = $1 AND mi.deleted_at IS NULL AND mp.deleted_at IS NULL AND ($2 = '' OR mp.file LIKE $2 || '/%')`,
		section, folder)
	if err != nil {
		return nil, nil, fmt.Errorf("plexproject: list section %d: %w", section, err)
	}
	defer rows.Close()
	have, byPath := map[string]rowState{}, map[string]rowState{}
	for rows.Next() {
		var extra, file string
		if err := rows.Scan(&extra, &file); err != nil {
			return nil, nil, err
		}
		st := rowState{uid: extraValue(extra, FileUIDKey), seedKey: extraValue(extra, SeedKeyKey), file: file}
		if st.uid != "" {
			have[st.uid] = st
		} else {
			byPath[file] = st
		}
	}
	return have, byPath, rows.Err()
}

func underFolder(path, folder string) bool {
	return folder == "" || strings.HasPrefix(path, folder+"/")
}

func (p *Projector) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}
