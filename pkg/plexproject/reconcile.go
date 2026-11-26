package plexproject

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// errNoLogger is what a type nobody handed a Logger answers where its work
// starts (the 2026-11-10 structured-logging design §5): it invents none.
var errNoLogger = errors.New("plexproject: Logger is required")

// ErrEmptyWant is a reconcile asked to empty a section that has stamped
// rows: an empty want list is what a watch that has not synced looks
// like, never a library that lost every file at once (review C4).
var ErrEmptyWant = errors.New("plexproject: an empty want list would empty the section; refused")

// Outcome counts what a reconcile did.
type Outcome struct {
	Applied, Deleted, Adopted, Unknown, Failed int
}

// rowState is one live media row of the section: its stamps, if any, and
// its file.
type rowState struct {
	uid, seedKey, itemKey, seedVersion, file string
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
	if p.Logger == nil {
		return Outcome{}, errNoLogger
	}
	var out Outcome
	have, byPath, err := p.liveRows(ctx, section, folder)
	if err != nil {
		return out, err
	}
	if len(want) == 0 && len(have) > 0 {
		return out, ErrEmptyWant
	}
	// want is the whole section's files whatever the folder: a row whose
	// file moved out of the folder is still wanted, never deleted
	// (review I8); only applies are limited to the folder.
	wanted := map[string]bool{}
	for _, c := range want {
		wanted[c.UID] = true
	}
	for _, c := range want {
		if !underFolder(c.PlexPath, folder) || c.Unresolved {
			continue
		}
		st, ok := have[c.UID]
		if ok && c.current(st.seedKey, st.itemKey, st.seedVersion) {
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
			p.Logger.WarnContext(ctx, "plexproject: apply during reconcile", "file", c.UID, "path", c.PlexPath, "err", err)
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
			p.Logger.WarnContext(ctx, "plexproject: delete during reconcile", "file", uid, "err", err)
			continue
		}
		out.Deleted++
	}
	out.Unknown = len(byPath)
	if folder == "" {
		p.refreshNeverRefreshed(ctx, section)
	}
	return out, nil
}

// refreshNeverRefreshed asks for the refresh of every live movie, show and
// season of the section Plex has never refreshed, and of the season of an
// episode it never has ([refreshTarget]). The projection asks once, when
// it inserts a row, and the watcher holds what it was asked in memory: a
// restart before the refresh was sent lost it, and a row an older build
// seeded whole was never asked for at all. Such a row has what the
// projection wrote and none of what only a refresh brings -- its extras
// (talos, 2026-11-17: 275 movies made at 07:34Z had no trailers while the
// provider held them, until each was refreshed by hand). A whole-section
// reconcile runs when the watch has synced, every SeedResync and at each
// scan of the section.
//
// A row is asked for once under the guid it carries: one still unrefreshed
// at a later reconcile is one Plex could not refresh -- the provider does
// not answer for it -- and asking again changes nothing until the row is
// re-keyed or the provider's answer does, so it waits RefreshAgain (talos,
// 2026-11-26: a show and its two seasons keyed by Plex ids two Series
// recorded were asked for at every reconcile, and Plex's refreshes of the
// seasons never ended). What was asked is held in memory, so a restart
// asks once more. A failed read is logged: the reconcile itself is done.
func (p *Projector) refreshNeverRefreshed(ctx context.Context, section int64) {
	if p.Refresh == nil {
		return
	}
	rows, err := p.DB.Query(ctx, `SELECT t.id, t.guid::text FROM metadata_items t WHERE t.id IN (
			SELECT CASE WHEN metadata_type = $2 AND parent_id IS NOT NULL THEN parent_id ELSE id END
			FROM metadata_items
			WHERE library_section_id = $1 AND deleted_at IS NULL AND refreshed_at IS NULL AND metadata_type = ANY($3))
		ORDER BY 1`,
		section, int(TypeEpisode), []int{int(TypeMovie), int(TypeShow), int(TypeSeason), int(TypeEpisode)})
	if err != nil {
		p.Logger.WarnContext(ctx, "plexproject: list the items Plex never refreshed", "section", section, "err", err)
		return
	}
	defer rows.Close()
	type row struct {
		id   int64
		guid string
	}
	var never []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.guid); err != nil {
			p.Logger.WarnContext(ctx, "plexproject: list the items Plex never refreshed", "section", section, "err", err)
			return
		}
		never = append(never, r)
	}
	if err := rows.Err(); err != nil {
		p.Logger.WarnContext(ctx, "plexproject: list the items Plex never refreshed", "section", section, "err", err)
		return
	}
	again := p.RefreshAgain
	if again <= 0 {
		again = 24 * time.Hour
	}
	now := p.now()
	var ids []int64
	p.askedMu.Lock()
	was := p.asked[section]
	asked := make(map[int64]askedRefresh, len(never))
	for _, r := range never {
		if a, ok := was[r.id]; ok && a.guid == r.guid && now.Sub(a.at) < again {
			asked[r.id] = a
			continue
		}
		asked[r.id] = askedRefresh{guid: r.guid, at: now}
		ids = append(ids, r.id)
	}
	if p.asked == nil {
		p.asked, p.held = map[int64]map[int64]askedRefresh{}, map[int64]int{}
	}
	// A row no longer listed was refreshed, trashed or deleted: forgotten.
	p.asked[section] = asked
	held := len(never) - len(ids)
	heldBefore := p.held[section]
	p.held[section] = held
	p.askedMu.Unlock()

	for _, id := range ids {
		p.Refresh.NeedsRefresh(id)
	}
	if len(ids) > 0 {
		p.Logger.InfoContext(ctx, "plexproject: asked for the refresh of items Plex never refreshed", "section", section, "items", len(ids))
	}
	if held != heldBefore && held > 0 {
		p.Logger.InfoContext(ctx, "plexproject: items Plex was asked to refresh and left unrefreshed are not asked for again yet",
			"section", section, "items", held, "again", again.String())
	}
}

// liveRows reads the section's (or the folder's) live media rows: the
// stamped ones by UID, the stamp-less ones by file.
func (p *Projector) liveRows(ctx context.Context, section int64, folder string) (map[string]rowState, map[string]rowState, error) {
	rows, err := p.DB.Query(ctx, `SELECT coalesce(mi.extra_data, ''), mp.file FROM media_items mi JOIN media_parts mp ON mp.media_item_id = mi.id
		WHERE mi.library_section_id = $1 AND mi.deleted_at IS NULL AND mp.deleted_at IS NULL AND ($2 = '' OR left(mp.file, length($2) + 1) = $2 || '/')`,
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
		m := extraMap(extra)
		st := rowState{uid: m[FileUIDKey], seedKey: m[SeedKeyKey], itemKey: m[ItemKeyKey], seedVersion: m[SeedVersionKey], file: file}
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
