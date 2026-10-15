package main

import (
	"context"
	"strconv"
	"sync/atomic"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
)

// recordSections keeps the sections provisioning reports, by Plex key,
// with the location id the database holds for each (location, by key); a
// section whose location cannot be read is left out, so the projection
// never writes under a location of zero.
func (m *Manager) recordSections(byName map[string]string, location func(key string) (int64, error)) {
	out := map[string]clustarrwatch.SectionIDs{}
	for name, key := range byName {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			m.Logger.Warn("provisioned section has no numeric key", "library", name, "key", key)
			continue
		}
		loc, err := location(key)
		if err != nil {
			m.Logger.Warn("provisioned section has no location yet", "library", name, "key", key, "error", err)
			continue
		}
		out[key] = clustarrwatch.SectionIDs{Section: id, Location: loc}
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

// locationOf reads a section's first location id from the library.
func (m *Manager) locationOf(ctx context.Context) func(key string) (int64, error) {
	return func(key string) (int64, error) {
		var id int64
		err := m.pool.QueryRow(ctx, `SELECT id FROM section_locations WHERE library_section_id = $1::bigint ORDER BY id LIMIT 1`, key).Scan(&id)
		return id, err
	}
}

// sectionRecord is the atomic the two above share; it lives here so the
// Manager struct's field list stays readable.
type sectionRecord = atomic.Pointer[map[string]clustarrwatch.SectionIDs]
