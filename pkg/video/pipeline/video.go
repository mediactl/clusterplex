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
	c         segmenter
	sd        *ffgo.StreamDecoder
	refused   refusals
	gpuDecode bool
	tenBit    bool
	// What the source needs that Plex's argv does not ask on the Pi
	// (findings H and D): a PQ or HLG source is tone-mapped to BT.709, an
	// interlaced one deinterlaced.
	toneMap, deinterlace bool
	transfer             string // the source's, for zscale
	// colourKnown is set once a frame's own colour has settled toneMap: a
	// container may name only the matrix, the transfer living in the
	// bitstream alone.
	colourKnown bool
	graph       *ffgo.FilterGraph
	// burn draws the job's subtitle (nil: none); a burn-in job is filtered
	// on the CPU, and on a GPU pod upload then takes its frames to QSV.
	burn   *burner
	upload *ffgo.FilterGraph
	enc    *ffgo.VideoStreamEncoder
	forced int

	// The grid (finding R): output frame j is at job.Start + j frames, and
	// takes the source frame nearest it, as -fps_mode cfr does.
	prev  ffgo.Frame
	prevT time.Duration
	have  bool
	slot  int64
}

func newVideoPath(job video.Job, in *input, o Options, devs *devices, c segmenter, burn *burner) (*videoPath, error) {
	vs := in.video
	bits := 8
	if name := ffgo.PixelFormatName(vs.PixelFmt); strings.Contains(name, "10") || strings.HasPrefix(name, "p010") {
		bits = 10
	}
	v := &videoPath{
		job: job, o: o, in: in, devs: devs, c: c, tenBit: bits == 10, burn: burn,
		forced: video.FirstForced(job.Start, job.Video.KeyframeEvery),
	}
	v.colour(vs.Color.Transfer)
	// Until a frame says, a 10-bit BT.2020 source of no stated transfer
	// may be HDR: it is decoded where its tone map can run.
	mayBeHDR := v.toneMap || (bits == 10 && vs.Color.Transfer == ffgo.ColorTransferUnspecified &&
		(vs.Color.Space == ffgo.ColorSpaceBT2020NCL || vs.Color.Space == ffgo.ColorSpaceBT2020CL))
	switch vs.FieldOrder {
	case ffgo.FieldOrderTT, ffgo.FieldOrderBB, ffgo.FieldOrderTB, ffgo.FieldOrderBT:
		v.deinterlace = true
	}
	v.deinterlace = v.deinterlace || job.Video.Deinterlace
	// Frames decoded on the GPU stay there, so they are decoded so only
	// where vpp_qsv does everything they need.
	v.gpuDecode = o.onGPU() && burn == nil && o.Caps.DecodesOnGPU(vs.Codec, bits) &&
		(!mayBeHDR || o.Caps.ToneMap) && (!v.deinterlace || o.Caps.Deinterlace)
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

// colour settles toneMap from a transfer characteristic: PQ and HLG are
// HDR, as is whatever Plex's argv asks to tone-map.
func (v *videoPath) colour(trc ffgo.ColorTransfer) {
	switch trc {
	case ffgo.ColorTransferSMPTE2084:
		v.toneMap, v.transfer = true, "smpte2084"
	case ffgo.ColorTransferARIB_STD_B67:
		v.toneMap, v.transfer = true, "arib-std-b67"
	default:
		v.toneMap = v.job.Video.ToneMap
	}
}

func (v *videoPath) close() {
	if v.have {
		_ = v.prev.Free()
	}
	if v.enc != nil {
		_ = v.enc.Close()
	}
	if v.upload != nil {
		_ = v.upload.Close()
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

func (v *videoPath) send(p *ffgo.Packet) error { return decode(v.sd, p, v.frame, &v.refused) }

func (v *videoPath) flush() error {
	if err := decode(v.sd, nil, v.frame, &v.refused); err != nil {
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
		if outs, err = v.post(outs); err != nil {
			return err
		}
		if err := v.encode(outs); err != nil {
			return err
		}
	}
	if v.upload != nil {
		outs, err := v.upload.Flush()
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
	if !v.colourKnown {
		// The first frame, before any filter or encoder is built.
		v.colourKnown = true
		if trc := f.ColorSpec().Transfer; trc != ffgo.ColorTransferUnspecified && trc != 0 {
			v.colour(trc)
		}
	}
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
	if outs, err = v.post(outs); err != nil {
		return err
	}
	return v.encode(outs)
}

// post draws an image subtitle onto the CPU chain's frames and, on a GPU
// pod, uploads a burn-in job's frames for h264_qsv. It frees every frame
// it replaces, and all of them on an error.
func (v *videoPath) post(outs []*ffgo.Frame) ([]*ffgo.Frame, error) {
	if v.burn == nil {
		return outs, nil
	}
	fail := func(err error, frames []*ffgo.Frame) ([]*ffgo.Frame, error) {
		for _, f := range frames {
			_ = f.Free()
		}
		return nil, err
	}
	if v.burn.sd != nil {
		for _, o := range outs {
			if err := composite(*o, v.burn.at(toTime(o.PTS(), outTB)), v.burn.canvasW, v.burn.canvasH); err != nil {
				return fail(err, outs)
			}
		}
	}
	if !v.o.onGPU() {
		return outs, nil
	}
	var up []*ffgo.Frame
	for i, o := range outs {
		if v.upload == nil {
			g, err := ffgo.NewFilterGraph(ffgo.FilterGraphConfig{
				Width: v.job.Video.Width, Height: v.job.Video.Height, PixelFmt: ffgo.PixelFormat(o.Format()),
				TimeBase: outTB, SAR: v.in.video.SampleAspectRatio, HWDevice: v.devs.qsv,
				Filters: "format=nv12,hwupload=extra_hw_frames=64,vpp_qsv=format=nv12",
			})
			if err != nil {
				return fail(fmt.Errorf("upload filters: %w", err), append(up, outs[i:]...))
			}
			v.upload = g
		}
		fs, err := v.upload.Filter(o)
		_ = o.Free()
		if err != nil {
			return fail(fmt.Errorf("upload: %w", err), append(up, outs[i+1:]...))
		}
		up = append(up, fs...)
	}
	return up, nil
}

func (v *videoPath) graphConfig(g ffgo.Frame) ffgo.FilterGraphConfig {
	vs, out := v.in.video, v.job.Video
	cfg := ffgo.FilterGraphConfig{
		Width: vs.Width, Height: vs.Height, PixelFmt: ffgo.PixelFormat(g.Format()),
		TimeBase: outTB, SAR: vs.SampleAspectRatio, HWFramesCtx: g.HWFramesCtx(),
	}
	// On a GPU pod, vpp_qsv deinterlaces and tone-maps where the device
	// measured it could; anything else runs on the CPU before the upload. A
	// burn-in job's chain is the CPU's alone, its subtitle drawn at the
	// output's size.
	cpuOnly := !v.o.onGPU() || v.burn != nil
	qsvDeint := !cpuOnly && v.deinterlace && v.o.Caps.Deinterlace
	qsvTone := !cpuOnly && v.toneMap && v.o.Caps.ToneMap
	vpp := fmt.Sprintf("vpp_qsv=w=%d:h=%d:format=nv12", out.Width, out.Height)
	if qsvTone {
		vpp = "vpp_qsv=tonemap=1:" + vpp[len("vpp_qsv="):]
	}
	if qsvDeint {
		vpp = "vpp_qsv=deinterlace=2:" + vpp[len("vpp_qsv="):]
	}
	var cpu []string
	if v.deinterlace && !qsvDeint {
		cpu = append(cpu, "bwdif=mode=send_frame")
	}
	if cpuOnly {
		cpu = append(cpu, fmt.Sprintf("scale=w=%d:h=%d", out.Width, out.Height))
	}
	if v.toneMap && !qsvTone {
		cpu = append(cpu, toneMapChain(v.transfer))
	}
	if v.burn != nil && v.burn.filter != "" {
		cpu = append(cpu, v.burn.filter)
	}
	switch {
	case cpuOnly:
		cfg.Filters = strings.Join(append(cpu, "format=yuv420p"), ",")
	case v.gpuDecode:
		cfg.Filters = "hwmap=derive_device=qsv,format=qsv," + vpp
	default:
		up := "nv12"
		if v.tenBit && (!v.toneMap || qsvTone) {
			up = "p010le"
		}
		cfg.Filters = strings.Join(append(cpu, "format="+up, "hwupload=extra_hw_frames=64", vpp), ",")
		cfg.HWDevice = v.devs.qsv
	}
	return cfg
}

// toneMapChain maps a PQ or HLG BT.2020 picture to BT.709 SDR on the CPU:
// to linear light at 100 nits, Hable's curve, back to BT.709.
func toneMapChain(transfer string) string {
	if transfer == "" {
		transfer = "smpte2084"
	}
	return "zscale=tin=" + transfer + ":pin=bt2020:min=bt2020nc:t=linear:npl=100,format=gbrpf32le," +
		"zscale=p=bt709,tonemap=hable,zscale=t=bt709:m=bt709:r=tv"
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
		GlobalHeader:   v.c.globalHeader(),
	}
	if v.o.onGPU() {
		last := v.graph
		if v.upload != nil {
			last = v.upload
		}
		cfg.HWFramesCtx = last.OutputHWFramesCtx()
		cfg.Bitrate = out.MaxRate * 8 / 10
		cfg.CodecOptions = map[string]string{"bf": "0", "forced_idr": "1", "async_depth": "1"}
	} else {
		cfg.PixelFormat = ffgo.PixelFormatYUV420P
		cfg.RateControl, cfg.CRF = ffgo.RateControlCRF, out.CRF
		cfg.CodecOptions = map[string]string{"bf": "0", "forced-idr": "1", "x264-params": "scenecut=0"}
	}
	if v.toneMap {
		// The stream says what the tone map made of it.
		cfg.CodecOptions["color_primaries"] = "bt709"
		cfg.CodecOptions["color_trc"] = "bt709"
		cfg.CodecOptions["colorspace"] = "bt709"
	}
	enc, err := ffgo.NewVideoStreamEncoder(cfg)
	if err != nil {
		return fmt.Errorf("%s: %w", v.o.Encoder, err)
	}
	v.enc = enc
	return v.c.openVideo(enc)
}

// maxRefusals packets or frames refused in a row fail a job: a decoder
// that refuses everything has met no damaged packet but a stream it cannot
// decode.
const maxRefusals = 500

// refusals counts what a decoder refused in a row and tells Skipped.
type refusals struct {
	inRow   int
	skipped func(error)
}

func (r *refusals) refused(err error) error {
	r.inRow++
	if r.inRow > maxRefusals {
		return fmt.Errorf("decode: %d in a row refused: %w", r.inRow, err)
	}
	if r.skipped != nil {
		r.skipped(err)
	}
	return nil
}

// decode sends p (nil at the end) and hands each frame to each, which the
// frame is freed after. A packet or frame the decoder refuses is skipped,
// as ffmpeg(1) skips it without -xerror -- a damaged TS packet after a
// network hiccup, one bad AAC frame in a file -- and counted in r.
func decode(sd *ffgo.StreamDecoder, p *ffgo.Packet, each func(ffgo.Frame) error, r *refusals) error {
	for {
		err := sd.Send(p)
		if err != nil && !errors.Is(err, ffgo.ErrAgain) && !errors.Is(err, io.EOF) {
			if rerr := r.refused(err); rerr != nil {
				return rerr
			}
		}
		for {
			f, rerr := sd.Receive()
			if errors.Is(rerr, ffgo.ErrAgain) || errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				if rerr = r.refused(rerr); rerr != nil {
					return rerr
				}
				break
			}
			r.inRow = 0
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
