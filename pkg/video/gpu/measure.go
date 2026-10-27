package gpu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/obinnaokechukwu/ffgo"
)

// Caps is what a pod's device was measured to do.
type Caps struct {
	Encode      bool // h264_qsv encodes
	Scale       bool // vpp_qsv scales and converts
	Deinterlace bool // vpp_qsv deinterlaces
	ToneMap     bool // vpp_qsv maps HDR10 to SDR
	// Decode is each "<codec>:<bits>" VA-API decoded on the GPU; any other
	// format is decoded on the CPU and uploaded.
	Decode map[string]bool
	// Took is how long the measure ran.
	Took time.Duration
}

// DecodesOnGPU reports whether a source of codec at bits decodes on the GPU.
func (c Caps) DecodesOnGPU(codec string, bits int) bool {
	return c.Decode[fmt.Sprintf("%s:%d", codec, bits)]
}

// decodeTrials are the formats clustarr's pods measured on the NUC
// (h264:8, hevc:8, hevc:10, mpeg2video:8, vp9:8, vp9:10).
var decodeTrials = []struct {
	codec string
	bits  int
}{{"h264", 8}, {"hevc", 8}, {"hevc", 10}, {"mpeg2video", 8}, {"vp9", 8}, {"vp9", 10}}

// Measure proves the Intel device at renderNode: an h264_qsv encode of a
// clip uploaded through vpp_qsv, the deinterlace and tone-map filters, and
// a VA-API decode of each format. It fails, with zero Caps, when the device
// does not open or cannot encode and scale: such a pod stays unready.
func Measure(ctx context.Context, renderNode string) (Caps, error) {
	began := time.Now()
	if err := IsRenderNode(renderNode); err != nil {
		return Caps{}, err
	}
	vaapi, err := ffgo.NewHWDevice(ffgo.HWDeviceTypeVAAPI, renderNode)
	if err != nil {
		return Caps{}, fmt.Errorf("open %s for VA-API: %w", renderNode, err)
	}
	defer func() { _ = vaapi.Close() }()
	qsv, err := ffgo.NewHWDevice(ffgo.HWDeviceTypeQSV, renderNode)
	if err != nil {
		return Caps{}, fmt.Errorf("open %s for QSV: %w", renderNode, err)
	}
	defer func() { _ = qsv.Close() }()

	h264, err := Clip(ctx, "h264", 8, time.Second, 1920, 1080)
	if err != nil {
		return Caps{}, err
	}
	if err := encodeOn(qsv, h264, "hwupload=extra_hw_frames=64,vpp_qsv=w=1280:h=720:format=nv12"); err != nil {
		return Caps{}, fmt.Errorf("h264_qsv through vpp_qsv: %w", err)
	}
	caps := Caps{Encode: true, Scale: true, Decode: map[string]bool{}}
	caps.Deinterlace = encodeOn(qsv, h264, "hwupload=extra_hw_frames=64,vpp_qsv=deinterlace=2:format=nv12") == nil
	hdr, err := Clip(ctx, "hevc", 10, time.Second, 1920, 1080)
	if err == nil {
		caps.ToneMap = encodeOn(qsv, hdr, "hwupload=extra_hw_frames=64,vpp_qsv=tonemap=1:format=nv12") == nil
	}
	for _, d := range decodeTrials {
		clip, err := Clip(ctx, d.codec, d.bits, time.Second, 640, 360)
		if err != nil {
			return Caps{}, err
		}
		caps.Decode[fmt.Sprintf("%s:%d", d.codec, d.bits)] = decodeOn(vaapi, clip) == nil
	}
	caps.Took = time.Since(began)
	return caps, nil
}

// IsRenderNode fails unless path is a character device: FFmpeg takes any
// other VA-API device string for an X11 display, and BtbN's build aborts the
// process where libX11 is absent (the pool's image has none).
func IsRenderNode(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("render node: %w", err)
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("render node %s is no character device", path)
	}
	return nil
}

// decodeOn decodes clip's video on dev and fails unless frames come out.
func decodeOn(dev *ffgo.HWDevice, clip []byte) error {
	n := 0
	err := decode(clip, &ffgo.StreamDecoderConfig{HWDevice: dev, ExtraHWFrames: 16}, func(ffgo.Frame) error { n++; return nil })
	if err == nil && n == 0 {
		err = errors.New("no frame decoded")
	}
	return err
}

// encodeOn decodes clip on the CPU, runs it through filters on dev and
// encodes it with h264_qsv, failing unless packets come out.
func encodeOn(dev *ffgo.HWDevice, clip []byte, filters string) error {
	var (
		graph   *ffgo.FilterGraph
		enc     *ffgo.VideoStreamEncoder
		packets int
	)
	defer func() {
		if enc != nil {
			_ = enc.Close()
		}
		if graph != nil {
			_ = graph.Close()
		}
	}()
	emit := func(*ffgo.Packet) error { packets++; return nil }
	var w, h int
	var tb ffgo.Rational
	err := decodeWith(clip, nil, func(si *ffgo.StreamInfo, sd *ffgo.StreamDecoder) { w, h, tb = si.Width, si.Height, sd.TimeBase() },
		func(f ffgo.Frame) error {
			var err error
			if graph == nil {
				if graph, err = ffgo.NewFilterGraph(ffgo.FilterGraphConfig{
					Width: w, Height: h, PixelFmt: ffgo.PixelFormat(f.Format()), TimeBase: tb,
					Filters: filters, HWDevice: dev,
				}); err != nil {
					return fmt.Errorf("filters: %w", err)
				}
			}
			out, err := graph.Filter(&f)
			if err != nil {
				return fmt.Errorf("filter: %w", err)
			}
			defer func() {
				for _, o := range out {
					_ = o.Free()
				}
			}()
			for _, o := range out {
				if enc == nil {
					if enc, err = ffgo.NewVideoStreamEncoder(ffgo.VideoStreamEncoderConfig{
						VideoEncoderConfig: ffgo.VideoEncoderConfig{
							EncoderName: "h264_qsv", Width: 1280, Height: 720, FrameRate: ffgo.NewRational(clipFPS, 1),
							HWFramesCtx: graph.OutputHWFramesCtx(), MaxBitrate: 3_000_000, BufferSize: 6_000_000, Bitrate: 2_500_000,
						},
						TimeBase: tb,
					}); err != nil {
						return fmt.Errorf("encoder: %w", err)
					}
				}
				if err := enc.Encode(*o, emit); err != nil {
					return fmt.Errorf("encode: %w", err)
				}
			}
			return nil
		})
	if err != nil {
		return err
	}
	if enc == nil {
		return errors.New("no frame reached the encoder")
	}
	if err := enc.Flush(emit); err != nil {
		return err
	}
	if packets == 0 {
		return errors.New("no packet encoded")
	}
	return nil
}

func decode(clip []byte, cfg *ffgo.StreamDecoderConfig, each func(ffgo.Frame) error) error {
	return decodeWith(clip, cfg, func(*ffgo.StreamInfo, *ffgo.StreamDecoder) {}, each)
}

// decodeWith decodes clip's video stream, telling opened its stream and
// decoder before the first frame, and handing every frame to each, which
// frees it after.
func decodeWith(clip []byte, cfg *ffgo.StreamDecoderConfig, opened func(*ffgo.StreamInfo, *ffgo.StreamDecoder), each func(ffgo.Frame) error) error {
	d, err := ffgo.NewDecoderFromReader(bytes.NewReader(clip), "matroska")
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	vs := d.VideoStream()
	if vs == nil {
		return errors.New("no video stream")
	}
	if cfg == nil {
		cfg = &ffgo.StreamDecoderConfig{}
	}
	sd, err := d.NewStreamDecoder(vs.Index, cfg)
	if err != nil {
		return fmt.Errorf("decoder: %w", err)
	}
	defer func() { _ = sd.Close() }()
	opened(vs, sd)
	drain := func() error {
		for {
			f, err := sd.Receive()
			if errors.Is(err, ffgo.ErrAgain) || errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("decode: %w", err)
			}
			err = each(f)
			_ = f.Free()
			if err != nil {
				return err
			}
		}
	}
	send := func(p *ffgo.Packet) error {
		for {
			err := sd.Send(p)
			if !errors.Is(err, ffgo.ErrAgain) {
				return err
			}
			if err := drain(); err != nil {
				return err
			}
		}
	}
	for {
		p, err := d.ReadPacket()
		if err != nil {
			return err
		}
		if p == nil {
			break
		}
		if p.StreamIndex() != vs.Index {
			continue
		}
		if err := send(p); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		if err := drain(); err != nil {
			return err
		}
	}
	if err := send(nil); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return drain()
}
