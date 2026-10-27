package pipeline

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/obinnaokechukwu/ffgo/avutil"

	"github.com/mediactl/clusterplex/pkg/video"
)

// outTB is the output's time base: fine enough for any grid.
var outTB = ffgo.NewRational(1, 90000)

// videoPath decodes the mapped video, puts it on the job's constant-rate
// grid, scales it and encodes H.264 with Plex's forced keyframes.
type videoPath struct {
	job       video.Job
	o         Options
	in        *input
	devs      *devices
	c         *cutter
	sd        *ffgo.StreamDecoder
	gpuDecode bool
	tenBit    bool
	graph     *ffgo.FilterGraph
	enc       *ffgo.VideoStreamEncoder
	forced    int

	// The grid (finding R): output frame j is at job.Start + j frames, and
	// takes the source frame nearest it, as -fps_mode cfr does.
	prev  ffgo.Frame
	prevT time.Duration
	have  bool
	slot  int64
}

func newVideoPath(job video.Job, in *input, o Options, devs *devices, c *cutter) (*videoPath, error) {
	vs := in.video
	bits := 8
	if name := ffgo.PixelFormatName(vs.PixelFmt); strings.Contains(name, "10") || strings.HasPrefix(name, "p010") {
		bits = 10
	}
	v := &videoPath{
		job: job, o: o, in: in, devs: devs, c: c, tenBit: bits == 10,
		gpuDecode: o.onGPU() && o.Caps.DecodesOnGPU(vs.Codec, bits),
		forced:    video.FirstForced(job.Start, job.Video.KeyframeEvery),
	}
	cfg := &ffgo.StreamDecoderConfig{}
	if v.gpuDecode {
		cfg = &ffgo.StreamDecoderConfig{HWDevice: devs.vaapi, ExtraHWFrames: 16}
	}
	sd, err := in.d.NewStreamDecoder(vs.Index, cfg)
	if err != nil {
		return nil, fmt.Errorf("video decoder: %w", err)
	}
	v.sd = sd
	return v, nil
}

func (v *videoPath) close() {
	if v.have {
		_ = v.prev.Free()
	}
	if v.enc != nil {
		_ = v.enc.Close()
	}
	if v.graph != nil {
		_ = v.graph.Close()
	}
	_ = v.sd.Close()
}

// slotTime is grid slot j's time.
func (v *videoPath) slotTime(j int64) time.Duration {
	r := v.job.Video.FrameRate
	return v.job.Start + time.Duration(j*int64(time.Second)*int64(r.Den)/int64(r.Num))
}

func (v *videoPath) frameDur() time.Duration { return v.slotTime(1) - v.slotTime(0) }

func (v *videoPath) send(p *ffgo.Packet) error { return decode(v.sd, p, v.frame) }

func (v *videoPath) flush() error {
	if err := decode(v.sd, nil, v.frame); err != nil {
		return err
	}
	if v.have {
		for v.slotTime(v.slot) < v.prevT+v.frameDur()/2 {
			if err := v.emit(v.prev, v.slot); err != nil {
				return err
			}
			v.slot++
		}
	}
	if v.graph != nil {
		outs, err := v.graph.Flush()
		if err != nil {
			return err
		}
		if err := v.encode(outs); err != nil {
			return err
		}
	}
	if v.enc == nil {
		return errors.New("no video frame reached the encoder")
	}
	return v.enc.Flush(v.c.video)
}

// frame takes a decoded frame onto the grid: every slot nearer the
// previous frame than this one is the previous frame's.
func (v *videoPath) frame(f ffgo.Frame) error {
	ts := f.PTS()
	if ts == avutil.AV_NOPTS_VALUE {
		ts = f.BestEffortTimestamp()
	}
	t := v.in.at(ts, v.sd.TimeBase())
	if v.have {
		mid := v.prevT + (t-v.prevT)/2
		for v.slotTime(v.slot) <= mid {
			if err := v.emit(v.prev, v.slot); err != nil {
				return err
			}
			v.slot++
		}
		_ = v.prev.Free()
	}
	c, err := f.Clone()
	if err != nil {
		return err
	}
	v.prev, v.prevT, v.have = c, t, true
	return nil
}

// emit is grid slot j, frame f's picture at the slot's time.
func (v *videoPath) emit(f ffgo.Frame, j int64) error {
	g, err := f.Clone()
	if err != nil {
		return err
	}
	defer func() { _ = g.Free() }()
	g.SetPTS(v.slotTime(j).Nanoseconds() * int64(outTB.Den) / int64(time.Second))
	if v.graph == nil {
		if v.graph, err = ffgo.NewFilterGraph(v.graphConfig(g)); err != nil {
			return fmt.Errorf("video filters: %w", err)
		}
	}
	outs, err := v.graph.Filter(&g)
	if err != nil {
		return fmt.Errorf("video filter: %w", err)
	}
	return v.encode(outs)
}

func (v *videoPath) graphConfig(g ffgo.Frame) ffgo.FilterGraphConfig {
	vs, out := v.in.video, v.job.Video
	cfg := ffgo.FilterGraphConfig{
		Width: vs.Width, Height: vs.Height, PixelFmt: ffgo.PixelFormat(g.Format()),
		TimeBase: outTB, SAR: vs.SampleAspectRatio, HWFramesCtx: g.HWFramesCtx(),
	}
	switch {
	case !v.o.onGPU():
		cfg.Filters = fmt.Sprintf("scale=w=%d:h=%d,format=yuv420p", out.Width, out.Height)
	case v.gpuDecode:
		cfg.Filters = fmt.Sprintf("hwmap=derive_device=qsv,format=qsv,vpp_qsv=w=%d:h=%d:format=nv12", out.Width, out.Height)
	default:
		up := "nv12"
		if v.tenBit {
			up = "p010le"
		}
		cfg.Filters = fmt.Sprintf("format=%s,hwupload=extra_hw_frames=64,vpp_qsv=w=%d:h=%d:format=nv12", up, out.Width, out.Height)
		cfg.HWDevice = v.devs.qsv
	}
	return cfg
}

// encode forces Plex's keyframes on the grid's times and encodes; every
// frame is freed.
func (v *videoPath) encode(outs []*ffgo.Frame) error {
	defer func() {
		for _, o := range outs {
			_ = o.Free()
		}
	}()
	for _, o := range outs {
		t := toTime(o.PTS(), outTB)
		if video.KeyframeDue(t, v.forced, v.job.Video.KeyframeEvery) {
			o.SetPictureType(ffgo.PictureTypeI)
			v.forced++
		} else {
			o.SetPictureType(ffgo.PictureTypeNone)
		}
		if v.enc == nil {
			if err := v.openEncoder(); err != nil {
				return err
			}
		}
		if err := v.enc.Encode(*o, v.c.video); err != nil {
			return fmt.Errorf("encode: %w", err)
		}
	}
	return nil
}

func (v *videoPath) openEncoder() error {
	out := v.job.Video
	every := int(out.KeyframeEvery.Seconds() * float64(out.FrameRate.Num) / float64(out.FrameRate.Den))
	cfg := ffgo.VideoStreamEncoderConfig{
		VideoEncoderConfig: ffgo.VideoEncoderConfig{
			EncoderName: v.o.Encoder, Width: out.Width, Height: out.Height,
			FrameRate: ffgo.NewRational(int32(out.FrameRate.Num), int32(out.FrameRate.Den)), GOPSize: 2 * every,
			MaxBitrate: out.MaxRate, BufferSize: out.BufSize, Preset: ffgo.EncoderPreset("veryfast"),
		},
		TimeBase:       outTB,
		KeepFrameTypes: true,
	}
	if v.o.onGPU() {
		cfg.HWFramesCtx = v.graph.OutputHWFramesCtx()
		cfg.Bitrate = out.MaxRate * 8 / 10
		cfg.CodecOptions = map[string]string{"bf": "0", "forced_idr": "1", "async_depth": "1"}
	} else {
		cfg.PixelFormat = ffgo.PixelFormatYUV420P
		cfg.RateControl, cfg.CRF = ffgo.RateControlCRF, out.CRF
		cfg.CodecOptions = map[string]string{"bf": "0", "forced-idr": "1", "x264-params": "scenecut=0"}
	}
	enc, err := ffgo.NewVideoStreamEncoder(cfg)
	if err != nil {
		return fmt.Errorf("%s: %w", v.o.Encoder, err)
	}
	v.enc = enc
	return v.c.openVideo(enc)
}

// decode sends p (nil at the end) and hands each frame to each, which the
// frame is freed after.
func decode(sd *ffgo.StreamDecoder, p *ffgo.Packet, each func(ffgo.Frame) error) error {
	for {
		err := sd.Send(p)
		if err != nil && !errors.Is(err, ffgo.ErrAgain) && !errors.Is(err, io.EOF) {
			return fmt.Errorf("decode: %w", err)
		}
		for {
			f, rerr := sd.Receive()
			if errors.Is(rerr, ffgo.ErrAgain) || errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				return fmt.Errorf("decode: %w", rerr)
			}
			rerr = each(f)
			_ = f.Free()
			if rerr != nil {
				return rerr
			}
		}
		if !errors.Is(err, ffgo.ErrAgain) {
			return nil
		}
	}
}
