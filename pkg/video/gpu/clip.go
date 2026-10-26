// Package gpu proves a pool pod's Intel device before it reports ready, as
// clustarr's pool does with inprocess.Engine.Measure: an h264_qsv encode, a
// vpp_qsv scale, and which formats VA-API decodes, each on clips encoded in
// memory. Only cmd/video-worker links it.
package gpu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/obinnaokechukwu/ffgo"
)

// clipEncoders make the clips the device is tried on.
var clipEncoders = map[string]string{
	"h264": "libx264", "hevc": "libx265", "mpeg2video": "mpeg2video", "vp9": "libvpx-vp9",
}

// clipOptions keep a clip quick to make.
var clipOptions = map[string]map[string]string{
	"libx264":    {"preset": "ultrafast"},
	"libx265":    {"preset": "ultrafast", "x265-params": "log-level=error"},
	"libvpx-vp9": {"deadline": "realtime", "cpu-used": "8"},
}

const clipFPS = 30

// Clip is d of codec at bits per sample (8 or 10), w x h at 30 fps,
// encoded on the CPU into Matroska in memory: noise for 8 bits, so an
// encoder has work to do, black for 10. Nothing touches the disk.
func Clip(ctx context.Context, codec string, bits int, d time.Duration, w, h int) ([]byte, error) {
	name, ok := clipEncoders[codec]
	if !ok {
		return nil, fmt.Errorf("no encoder for a %s clip", codec)
	}
	pf := ffgo.PixelFormatYUV420P
	switch bits {
	case 8:
	case 10:
		pf = ffgo.PixelFormatYUV420P10LE()
	default:
		return nil, fmt.Errorf("a %d-bit clip", bits)
	}
	var buf bytes.Buffer
	m, err := ffgo.NewMuxerToWriter(&buf, "matroska")
	if err != nil {
		return nil, err
	}
	defer func() { _ = m.Close() }()
	enc, err := ffgo.NewVideoStreamEncoder(ffgo.VideoStreamEncoderConfig{
		VideoEncoderConfig: ffgo.VideoEncoderConfig{
			EncoderName: name, Width: w, Height: h, PixelFormat: pf,
			FrameRate: ffgo.NewRational(clipFPS, 1), GOPSize: clipFPS, CodecOptions: clipOptions[name],
		},
		TimeBase:     ffgo.NewRational(1, clipFPS),
		GlobalHeader: m.NeedsGlobalHeader(),
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	defer func() { _ = enc.Close() }()
	ms, err := m.AddEncoderStream(enc, ffgo.StreamOptions{})
	if err != nil {
		return nil, err
	}
	if err := m.WriteHeader(); err != nil {
		return nil, err
	}
	var noise *ffgo.FilterGraph
	if bits == 8 {
		if noise, err = ffgo.NewFilterGraph(ffgo.FilterGraphConfig{
			Width: w, Height: h, PixelFmt: pf, TimeBase: ffgo.NewRational(1, clipFPS),
			Filters: "noise=alls=30:allf=t+u",
		}); err != nil {
			return nil, err
		}
		defer func() { _ = noise.Close() }()
	}
	write := func(p *ffgo.Packet) error { return m.WritePacket(ms, p) }
	for i := range int(d.Seconds() * clipFPS) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := clipFrame(pf, w, h, int64(i), noise, func(f ffgo.Frame) error { return enc.Encode(f, write) }); err != nil {
			return nil, err
		}
	}
	if err := enc.Flush(write); err != nil {
		return nil, err
	}
	if err := m.WriteTrailer(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// clipFrame makes frame n and hands it, through noise when there is one,
// to each; every frame is freed after.
func clipFrame(pf ffgo.PixelFormat, w, h int, n int64, noise *ffgo.FilterGraph, each func(ffgo.Frame) error) error {
	f, err := ffgo.NewVideoFrame(pf, w, h)
	if err != nil {
		return err
	}
	defer func() { _ = f.Free() }()
	f.SetPTS(n)
	if noise == nil {
		return each(f)
	}
	out, err := noise.Filter(&f)
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range out {
		errs = append(errs, each(*o))
		_ = o.Free()
	}
	return errors.Join(errs...)
}
