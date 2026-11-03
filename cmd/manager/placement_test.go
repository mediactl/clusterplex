package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mediactl/clusterplex/pkg/plex/activity"
	videorelay "github.com/mediactl/clusterplex/pkg/video/relay"
)

// A pool conversion is labelled with the hardware API Plex would name:
// QSV for the Intel pool's h264_qsv, none for a software encoder, and its
// worker's hardware only once the worker has reported it.
func TestAPoolConversionIsPlacedAsPlexNamesHardware(t *testing.T) {
	for name, tc := range map[string]struct {
		in   videorelay.Placement
		want activity.Placement
	}{
		"qsv on vaapi":  {videorelay.Placement{Node: "nuc", Encoder: "h264_qsv", Decoder: "vaapi"}, activity.Placement{Node: "nuc", Reported: true, HWDecode: "vaapi", HWEncode: "qsv"}},
		"cpu":           {videorelay.Placement{Node: "nuc", Encoder: "libx264"}, activity.Placement{Node: "nuc", Reported: true}},
		"not yet named": {videorelay.Placement{Node: "nuc"}, activity.Placement{Node: "nuc"}},
		"vaapi encode":  {videorelay.Placement{Encoder: "hevc_vaapi", Decoder: "vaapi"}, activity.Placement{Reported: true, HWDecode: "vaapi", HWEncode: "vaapi"}},
	} {
		assert.Equal(t, tc.want, placementOf(tc.in), name)
	}
}
