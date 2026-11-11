package pipeline_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/pipeline"
)

func TestEligibilityJudgesCodecSizeAndHDR(t *testing.T) {
	e := pipeline.Eligibility{Sources: []string{"h264", "mpeg2video"}, MaxPixels: 1920 * 1080, NoHDR: true}
	ok := pipeline.Source{Codec: "h264", Width: 1920, Height: 1080, Bits: 8}
	assert.NoError(t, e.Check(ok))
	// 1088 coded lines are 1080p: a height that is a multiple of 16 may be
	// the coded padding of a display height up to 15 lines shorter.
	assert.NoError(t, e.Check(pipeline.Source{Codec: "mpeg2video", Width: 1920, Height: 1088, Bits: 8}), "1088 coded lines are 1080p")
	for _, bad := range []pipeline.Source{
		{Codec: "hevc", Width: 1920, Height: 1080, Bits: 8},
		{Codec: "h264", Width: 3840, Height: 2160, Bits: 8},
		{Codec: "h264", Width: 1920, Height: 1080, Bits: 10, HDR: true},
	} {
		err := e.Check(bad)
		require.ErrorIs(t, err, video.ErrNotForPool, bad)
	}
	assert.EqualError(t, e.Check(pipeline.Source{Codec: "hevc", Width: 1920, Height: 1080}), "not for this pool: hevc")
	assert.EqualError(t, e.Check(pipeline.Source{Codec: "h264", Width: 3840, Height: 2160}), "not for this pool: 3840x2160")
	assert.EqualError(t, e.Check(pipeline.Source{Codec: "h264", Width: 1280, Height: 720, HDR: true}), "not for this pool: HDR")
	assert.NoError(t, pipeline.Eligibility{}.Check(pipeline.Source{Codec: "hevc", Width: 7680, Height: 4320, HDR: true}), "an empty rule takes everything")
	assert.Error(t, pipeline.Eligibility{NoHDR: true, Sources: []string{"h264"}}.Check(pipeline.Source{Codec: "h264", Width: 1280, Height: 720, HDR: true}))
	assert.NoError(t, pipeline.Eligibility{Sources: []string{"h264"}}.Check(pipeline.Source{Codec: "h264", Width: 1280, Height: 720, HDR: true}), "only NoHDR refuses HDR")
}
