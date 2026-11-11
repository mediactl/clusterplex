package main

import (
	"strings"

	"github.com/mediactl/clusterplex/pkg/video/pipeline"
)

// eligibility is the pod's rule over the sources it runs, from --sources
// (a comma list of codec names, trimmed, empties dropped),
// --max-source-pixels and --no-hdr; nil when none is set, so a pod with no
// rule judges nothing (the GPU pool's shape).
func eligibility(sources string, maxPixels int, noHDR bool) *pipeline.Eligibility {
	var codecs []string
	for c := range strings.SplitSeq(sources, ",") {
		if c = strings.TrimSpace(c); c != "" {
			codecs = append(codecs, c)
		}
	}
	if len(codecs) == 0 && maxPixels == 0 && !noHDR {
		return nil
	}
	return &pipeline.Eligibility{Sources: codecs, MaxPixels: maxPixels, NoHDR: noHDR}
}
