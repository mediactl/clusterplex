package plexproject

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// EnsureDirectory returns the directories row for dir, creating the chain
// from the section's root row down. Plex keeps one root row per section
// with an empty path and no parent (the capture doc), and under it one row
// per folder element, its path relative to the location's root and its
// parent the row above.
func EnsureDirectory(ctx context.Context, tx pgx.Tx, section int64, root, dir string, now time.Time) (int64, error) {
	rel, err := filepath.Rel(root, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return 0, fmt.Errorf("plexproject: %s is not a folder under %s", dir, root)
	}
	id, err := findOrInsertDirectory(ctx, tx, section, nil, "", now)
	if err != nil {
		return 0, fmt.Errorf("plexproject: root directory of section %d: %w", section, err)
	}
	if rel == "." {
		return id, nil // a file directly under the root hangs off the root row
	}
	acc := ""
	for _, el := range strings.Split(filepath.ToSlash(rel), "/") {
		if el == "" {
			continue
		}
		acc = path.Join(acc, el)
		parent := id
		if id, err = findOrInsertDirectory(ctx, tx, section, &parent, acc, now); err != nil {
			return 0, fmt.Errorf("plexproject: directory %q: %w", acc, err)
		}
	}
	return id, nil
}

func findOrInsertDirectory(ctx context.Context, tx pgx.Tx, section int64, parent *int64, p string, now time.Time) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM directories WHERE library_section_id = $1 AND path = $2 AND deleted_at IS NULL ORDER BY id LIMIT 1`, section, p).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `INSERT INTO directories (library_section_id, parent_directory_id, path, created_at, updated_at) VALUES ($1, $2, $3, $4, $4) RETURNING id`,
			section, parent, p, now.Unix()).Scan(&id)
	}
	return id, err
}
