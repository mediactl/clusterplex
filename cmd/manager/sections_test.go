package main

import (
	"errors"
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
	m.recordSections(map[string]string{"Clustarr Movies": "2", "Clustarr TV": "3"}, func(key string) (int64, error) {
		return map[string]int64{"2": 20, "3": 30}[key], nil
	})
	require.Equal(t, map[string]clustarrwatch.SectionIDs{"2": {Section: 2, Location: 20}, "3": {Section: 3, Location: 30}}, m.provisionedSections())
	m.recordSections(map[string]string{}, nil)
	require.Empty(t, m.provisionedSections())
}

// A section whose location the database cannot answer is left out, so the
// projection never writes under a location id of zero.
func TestASectionWithoutALocationIsNotProvisioned(t *testing.T) {
	m := newTestManager()
	m.recordSections(map[string]string{"Clustarr Movies": "2", "Clustarr TV": "x"}, func(key string) (int64, error) {
		if key == "2" {
			return 20, nil
		}
		return 0, errors.New("no location")
	})
	require.Equal(t, map[string]clustarrwatch.SectionIDs{"2": {Section: 2, Location: 20}}, m.provisionedSections())
}
