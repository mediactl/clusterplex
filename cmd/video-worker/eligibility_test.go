package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEligibilityComesFromTheFlags(t *testing.T) {
	assert.Nil(t, eligibility("", 0, false), "no flag: every source")
	e := eligibility("h264, mpeg2video,", 2073600, true)
	require.NotNil(t, e)
	assert.Equal(t, []string{"h264", "mpeg2video"}, e.Sources)
	assert.Equal(t, 2073600, e.MaxPixels)
	assert.True(t, e.NoHDR)
	assert.False(t, eligibility("h264", 0, false).NoHDR, "only --no-hdr refuses HDR")
	assert.Equal(t, 2073600, eligibility("", 2073600, false).MaxPixels, "a size alone is a rule")
	assert.True(t, eligibility("", 0, true).NoHDR, "--no-hdr alone is a rule")
}
