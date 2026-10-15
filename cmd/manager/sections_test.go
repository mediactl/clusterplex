package main

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
)

// The provisioned sections the projection owns follow the last
// provisioning run: each library's section key, with the location id the
// database holds for it; a run that reports none leaves none.
func TestProvisionedSectionsFollowTheLastProvisioningRun(t *testing.T) {
	m := newTestManager()
	require.Empty(t, m.provisionedSections(), "none before provisioning ran")
	m.recordSections(map[string]string{"Clustarr Movies": "2", "Clustarr TV": "3"}, func(key string) ([]clustarrwatch.SectionLocation, error) {
		return []clustarrwatch.SectionLocation{{ID: map[string]int64{"2": 20, "3": 30}[key], Root: "/media/" + key}}, nil
	})
	require.Equal(t, int64(20), m.provisionedSections()["2"].Location)
	require.Equal(t, int64(30), m.provisionedSections()["3"].Location)
	m.recordSections(map[string]string{}, nil)
	require.Empty(t, m.provisionedSections())
}

// A section whose location the database cannot answer is left out, so the
// projection never writes under a location id of zero.
func TestASectionWithoutALocationIsNotProvisioned(t *testing.T) {
	m := newTestManager()
	m.recordSections(map[string]string{"Clustarr Movies": "2", "Clustarr TV": "x"}, func(key string) ([]clustarrwatch.SectionLocation, error) {
		if key == "2" {
			return []clustarrwatch.SectionLocation{{ID: 20, Root: "/media/movies"}}, nil
		}
		return nil, errors.New("no location")
	})
	require.Equal(t, []string{"2"}, slices.Collect(maps.Keys(m.provisionedSections())))
}
