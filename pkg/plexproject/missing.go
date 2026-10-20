package plexproject

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mediactl/clusterplex/pkg/plexseed"
)

// MissingKey stamps a media row the sweep took off its item because its
// file was gone, with the Unix time it did: only a row carrying it is ever
// put back when the file returns.
const MissingKey = "cp:missing"

// ErrStalled is a file system that did not answer within DiskFiles'
// timeout: a hung NFS server. A sweep stops at it rather than wait on every
// file in turn while it holds the section.
var ErrStalled = errors.New("plexproject: the file system did not answer")

// Files is what the sweep asks of the disk.
type Files interface {
	// Root reports whether a library location's root can be trusted to say
	// a file under it is gone. An error means it cannot -- unreachable, or
	// listing empty, which is how an NFS mount that is not there looks --
	// and nothing under it is judged.
	Root(ctx context.Context, root string) error
	// Missing reports whether path is gone: true only for a definite "no
	// such file", an error for any answer that is not one.
	Missing(ctx context.Context, path string) (bool, error)
}

// DiskFiles is Files on this process's file system, where Plex's library
// paths are mounted.
type DiskFiles struct {
	// Timeout bounds each call (default 10s).
	Timeout time.Duration
	// Stat is os.Stat when nil.
	Stat func(string) (fs.FileInfo, error)
}

// Root is Files.Root: a directory that lists at least one entry.
func (d *DiskFiles) Root(ctx context.Context, root string) error {
	_, err := within(ctx, d.timeout(), func() (struct{}, error) {
		f, err := os.Open(root)
		if err != nil {
			return struct{}{}, err
		}
		defer func() { _ = f.Close() }()
		if _, err := f.Readdirnames(1); err != nil {
			if errors.Is(err, io.EOF) {
				return struct{}{}, fmt.Errorf("plexproject: %s lists nothing; is its mount there?", root)
			}
			return struct{}{}, err
		}
		return struct{}{}, nil
	})
	return err
}

// Missing is Files.Missing.
func (d *DiskFiles) Missing(ctx context.Context, path string) (bool, error) {
	stat := d.Stat
	if stat == nil {
		stat = os.Stat
	}
	_, err := within(ctx, d.timeout(), func() (fs.FileInfo, error) { return stat(path) })
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	default:
		return false, err
	}
}

func (d *DiskFiles) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return 10 * time.Second
}

// within runs f, giving up after timeout. A stat stuck in the kernel
// cannot be cancelled; its goroutine ends when the call does.
func within[T any](ctx context.Context, timeout time.Duration, f func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := f()
		ch <- result{v, err}
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	var zero T
	select {
	case r := <-ch:
		return r.v, r.err
	case <-t.C:
		return zero, ErrStalled
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// SweepScope limits a sweep: to the files under Folder, and to Item and
// the items under it (a show's seasons and episodes). Zero values are the
// whole section.
type SweepScope struct {
	Folder string
	Item   int64
}

// SweepOutcome counts what a sweep did. ProjectedMissing lists the files
// of rows the projection wrote that are gone: clustarr's catalog naming a
// file that is not there, which only clustarr can fix.
type SweepOutcome struct {
	Checked, Removed, Revived, Skipped int
	ProjectedMissing                   []string
}

// Sweep takes the gone files off a section's items, as Plex's own scanner
// did before the projection replaced it: Plex never does on a refresh. Only
// a row Plex's scanner made (no cp:mediaFile stamp) is ever removed, soft-
// deleted as the projection's Delete does so the empty-trash job decides
// when it is purged, and its item is trashed if nothing is left. A row the
// projection wrote follows its MediaFile and is only reported. A row the
// sweep removed whose file is back is put back, with its item. Nothing is
// judged under a root Files cannot trust, and a stalled file system stops
// the sweep. Without Files the sweep does nothing.
func (p *Projector) Sweep(ctx context.Context, section int64, scope SweepScope) (SweepOutcome, error) {
	var out SweepOutcome
	if p.Files == nil {
		return out, nil
	}
	roots, err := p.sectionRoots(ctx, section)
	if err != nil {
		return out, err
	}
	rows, err := p.sweepRows(ctx, section, scope.Item)
	if err != nil {
		return out, err
	}
	trusted := map[string]bool{}
	for _, r := range rows {
		if !anyUnder(r.files, scope.Folder) {
			continue
		}
		root := longestRoot(roots, r.files[0])
		ok, seen := trusted[root]
		if !seen {
			err := errors.New("no location covers the file")
			if root != "" {
				err = p.Files.Root(ctx, root)
			}
			if errors.Is(err, ErrStalled) {
				return out, err
			}
			if ok = err == nil; !ok {
				p.log().WarnContext(ctx, "plexproject: sweep: not judging files under an untrusted root", "section", section, "root", root, "error", err)
			}
			trusted[root] = ok
		}
		if !ok {
			out.Skipped++
			continue
		}
		gone, err := p.allGone(ctx, r.files)
		if errors.Is(err, ErrStalled) || ctx.Err() != nil {
			return out, errors.Join(err, ctx.Err())
		}
		if err != nil {
			out.Skipped++
			p.log().WarnContext(ctx, "plexproject: sweep: cannot tell whether a file is there", "file", r.files[0], "error", err)
			continue
		}
		out.Checked++
		switch {
		case r.projected:
			if gone {
				out.ProjectedMissing = append(out.ProjectedMissing, r.files[0])
			}
		case r.removed:
			if !gone {
				if err := p.revive(ctx, r); err != nil {
					return out, fmt.Errorf("plexproject: revive media %d: %w", r.media, err)
				}
				out.Revived++
			}
		case gone:
			done, err := p.removeGone(ctx, r)
			if err != nil {
				return out, fmt.Errorf("plexproject: remove media %d: %w", r.media, err)
			}
			if done {
				out.Removed++
			}
		}
	}
	return out, nil
}

// sweepRow is one media row the sweep looks at: a live one, or one it
// removed earlier (removed).
type sweepRow struct {
	media, item        int64
	extra              string
	files              []string
	projected, removed bool
}

func (p *Projector) sweepRows(ctx context.Context, section, item int64) ([]sweepRow, error) {
	rows, err := p.DB.Query(ctx, `SELECT mi.id, mi.metadata_item_id, coalesce(mi.extra_data, ''), mi.deleted_at IS NOT NULL,
		coalesce((SELECT array_agg(mp.file ORDER BY mp.id) FROM media_parts mp
			WHERE mp.media_item_id = mi.id AND coalesce(mp.file, '') <> '' AND (mp.deleted_at IS NULL OR mi.deleted_at IS NOT NULL)), '{}')
		FROM media_items mi
		WHERE mi.library_section_id = $1
		AND (mi.deleted_at IS NULL OR mi.extra_data LIKE '%"`+MissingKey+`":%')
		AND ($2::bigint = 0 OR mi.metadata_item_id IN (
			WITH RECURSIVE under(id) AS (SELECT $2::bigint UNION SELECT m.id FROM metadata_items m JOIN under ON m.parent_id = under.id)
			SELECT id FROM under))
		ORDER BY mi.id`, section, item)
	if err != nil {
		return nil, fmt.Errorf("plexproject: list section %d for the sweep: %w", section, err)
	}
	defer rows.Close()
	var out []sweepRow
	for rows.Next() {
		var r sweepRow
		var itemID *int64
		if err := rows.Scan(&r.media, &itemID, &r.extra, &r.removed, &r.files); err != nil {
			return nil, err
		}
		if len(r.files) == 0 || itemID == nil {
			continue // no file to judge, or no item to take it off
		}
		r.item = *itemID
		m := extraMap(r.extra)
		r.projected = m[FileUIDKey] != ""
		if r.removed && m[MissingKey] == "" {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// allGone reports whether every one of a row's files is gone; a row with
// one file still there is not.
func (p *Projector) allGone(ctx context.Context, files []string) (bool, error) {
	for _, f := range files {
		gone, err := p.Files.Missing(ctx, f)
		if err != nil || !gone {
			return false, err
		}
	}
	return true, nil
}

// removeGone soft-deletes a row Plex made whose file is gone, stamps it
// MissingKey, and trashes its item if nothing is left on it; the item's
// updated_at moves when it keeps another file. The update re-checks the row
// is still live and still Plex's, so a row an apply adopted meanwhile is
// left. done is false when it was.
func (p *Projector) removeGone(ctx context.Context, r sweepRow) (bool, error) {
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := p.now().Unix()
	m := extraMap(r.extra)
	m[MissingKey] = strconv.FormatInt(now, 10)
	tag, err := tx.Exec(ctx, `UPDATE media_items SET deleted_at = $2, updated_at = $2, extra_data = $3
		WHERE id = $1 AND deleted_at IS NULL AND coalesce(extra_data, '') NOT LIKE '%"`+FileUIDKey+`":%'`, r.media, now, plexseed.ExtraData(m))
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE media_parts SET deleted_at = $2, updated_at = $2 WHERE media_item_id = $1 AND deleted_at IS NULL`, r.media, now); err != nil {
		return false, err
	}
	if err := trashIfEmpty(ctx, tx, r.item, now); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE metadata_items SET updated_at = $2 WHERE id = $1 AND deleted_at IS NULL`, r.item, now); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// revive puts back a row the sweep removed whose file is there again --
// the parts it removed with it, its item and every parent it trashed --
// before the empty-trash job purged them.
func (p *Projector) revive(ctx context.Context, r sweepRow) error {
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := p.now().Unix()
	var removedAt int64
	err = tx.QueryRow(ctx, `SELECT deleted_at FROM media_items WHERE id = $1 AND deleted_at IS NOT NULL FOR UPDATE`, r.media).Scan(&removedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	m := extraMap(r.extra)
	delete(m, MissingKey)
	if _, err := tx.Exec(ctx, `UPDATE media_items SET deleted_at = NULL, updated_at = $2, extra_data = $3 WHERE id = $1`, r.media, now, plexseed.ExtraData(m)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE media_parts SET deleted_at = NULL, updated_at = $2 WHERE media_item_id = $1 AND deleted_at = $3`, r.media, now, removedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `WITH RECURSIVE up(id, parent_id) AS (
			SELECT id, parent_id FROM metadata_items WHERE id = $1
			UNION SELECT m.id, m.parent_id FROM metadata_items m JOIN up ON m.id = up.parent_id)
		UPDATE metadata_items SET deleted_at = NULL, updated_at = $2 WHERE id IN (SELECT id FROM up) AND deleted_at IS NOT NULL`, r.item, now); err != nil {
		return err
	}
	if err := countMedia(ctx, tx, r.item); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// sectionRoots are the section's location roots.
func (p *Projector) sectionRoots(ctx context.Context, section int64) ([]string, error) {
	rows, err := p.DB.Query(ctx, `SELECT root_path FROM section_locations WHERE library_section_id = $1`, section)
	if err != nil {
		return nil, fmt.Errorf("plexproject: section %d locations: %w", section, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		out = append(out, strings.TrimRight(root, "/"))
	}
	return out, rows.Err()
}

// longestRoot is the root that holds file, the longest when several do;
// "" when none does.
func longestRoot(roots []string, file string) string {
	best := ""
	for _, r := range roots {
		if r != "" && strings.HasPrefix(file, r+"/") && len(r) > len(best) {
			best = r
		}
	}
	return best
}

func anyUnder(files []string, folder string) bool {
	for _, f := range files {
		if underFolder(f, folder) {
			return true
		}
	}
	return false
}
