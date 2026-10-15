package main

import (
	"context"
	"strconv"
	"sync/atomic"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
)

// recordSections keeps the sections provisioning reports, by Plex key,
// with every location the database holds for each (locations, by key); a
// section whose locations cannot be read, or that has none, is left out,
// so the projection never writes under a location of zero.
func (m *Manager) recordSections(byName map[string]string, locations func(key string) ([]clustarrwatch.SectionLocation, error)) {
	out := map[string]clustarrwatch.SectionIDs{}
	for name, key := range byName {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			m.Logger.Warn("provisioned section has no numeric key", "library", name, "key", key)
			continue
		}
		locs, err := locations(key)
		if err != nil || len(locs) == 0 {
			m.Logger.Warn("provisioned section has no location yet", "library", name, "key", key, "error", err)
			continue
		}
		out[key] = clustarrwatch.SectionIDs{Section: id, Location: locs[0].ID, Locations: locs}
	}
	m.sections.Store(&out)
}

// provisionedSections is what recordSections last stored: the sections
// the projection owns (clustarrwatch.Watcher.Sections).
func (m *Manager) provisionedSections() map[string]clustarrwatch.SectionIDs {
	p := m.sections.Load()
	if p == nil {
		return map[string]clustarrwatch.SectionIDs{}
	}
	return *p
}

// locationsOf reads a section's locations from the library.
func (m *Manager) locationsOf(ctx context.Context) func(key string) ([]clustarrwatch.SectionLocation, error) {
	return func(key string) ([]clustarrwatch.SectionLocation, error) {
		rows, err := m.pool.Query(ctx, `SELECT id, root_path FROM section_locations WHERE library_section_id = $1::bigint ORDER BY id`, key)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []clustarrwatch.SectionLocation
		for rows.Next() {
			var l clustarrwatch.SectionLocation
			if err := rows.Scan(&l.ID, &l.Root); err != nil {
				return nil, err
			}
			out = append(out, l)
		}
		return out, rows.Err()
	}
}

// sectionRecord is the atomic the two above share; it lives here so the
// Manager struct's field list stays readable.
type sectionRecord = atomic.Pointer[map[string]clustarrwatch.SectionIDs]
