package clustarrwatch_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
)

func TestGuidIsTheFormClustarrsProviderIssues(t *testing.T) {
	assert.Equal(t, "tv.plex.agents.custom.clustarr.movies://movie/0b6c",
		clustarrwatch.Guid("tv.plex.agents.custom.clustarr.movies", "movie", "0b6c"))
}
