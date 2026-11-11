package pipeline

import (
	"fmt"
	"slices"
	"strings"

	"github.com/obinnaokechukwu/ffgo"

	"github.com/mediactl/clusterplex/pkg/video"
)

// Source is what a pod judges a job's video stream by once the input is
// open: the codec, the coded size and whether the stream is HDR (PQ or
// HLG, or 10-bit BT.2020 of no stated transfer, which may be).
type Source struct {
	Codec         string
	Width, Height int
	HDR           bool
}

// Eligibility is a pod's rule over the sources it runs (the CPU pool's:
// H.264 and MPEG-2 at 1080p or less, no HDR), judged before any output so
// the relay can pass the pod over for one beyond it. An empty field bounds
// nothing: no Sources takes any codec, MaxPixels 0 any size, and NoHDR false
// takes HDR, so the zero value takes everything.
type Eligibility struct {
	Sources   []string // codec names as FFmpeg names them (h264, mpeg2video, hevc)
	MaxPixels int      // the most display pixels a frame may carry
	NoHDR     bool     // refuse an HDR source
}

// Check is nil when s passes e, else video.ErrNotForPool naming the field
// that refused it: "not for this pool: hevc", "not for this pool:
// 3840x2160", "not for this pool: HDR".
func (e Eligibility) Check(s Source) error {
	if len(e.Sources) > 0 && !slices.Contains(e.Sources, s.Codec) {
		return fmt.Errorf("%w: %s", video.ErrNotForPool, s.Codec)
	}
	if e.MaxPixels > 0 && s.Width*displayHeight(s.Height) > e.MaxPixels {
		return fmt.Errorf("%w: %dx%d", video.ErrNotForPool, s.Width, s.Height)
	}
	if e.NoHDR && s.HDR {
		return fmt.Errorf("%w: HDR", video.ErrNotForPool)
	}
	return nil
}

// displayHeight is the shortest display height a coded height h may stand
// for: a codec pads a frame to a multiple of 16 lines, so 1088 coded lines
// are 1080 displayed, and a height on such a boundary is judged 16 lines
// short rather than refuse a 1080p stream over its padding. Any other
// height is its own.
func displayHeight(h int) int {
	if h > 16 && h%16 == 0 {
		return h - 16
	}
	return h
}

// sourceOf reads the open input's video stream as a Source.
func sourceOf(in *input) Source {
	vs := in.video
	bits := bitsOf(vs)
	return Source{
		Codec: vs.Codec, Width: vs.Width, Height: vs.Height,
		HDR: vs.Color.Transfer == ffgo.ColorTransferSMPTE2084 || vs.Color.Transfer == ffgo.ColorTransferARIB_STD_B67 || mayBeHDR(vs, bits),
	}
}

// bitsOf is a stream's bit depth as its pixel format names it: 10 for a
// 10-bit or p010 format, else 8.
func bitsOf(vs *ffgo.StreamInfo) int {
	if name := ffgo.PixelFormatName(vs.PixelFmt); strings.Contains(name, "10") || strings.HasPrefix(name, "p010") {
		return 10
	}
	return 8
}

// mayBeHDR is a 10-bit BT.2020 stream of no stated transfer: until a frame
// says, it may be HDR, so it is judged as HDR and decoded where its tone
// map can run.
func mayBeHDR(vs *ffgo.StreamInfo, bits int) bool {
	return bits == 10 && vs.Color.Transfer == ffgo.ColorTransferUnspecified &&
		(vs.Color.Space == ffgo.ColorSpaceBT2020NCL || vs.Color.Space == ffgo.ColorSpaceBT2020CL)
}
