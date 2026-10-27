// Package pipeline is the GPU pool's conversion, in process on ffgo: the
// input decoded on VA-API or the CPU, a constant-rate grid anchored at the
// job's -ss, Plex's scale on vpp_qsv (swscale where no GPU is), H.264 with
// Plex's forced keyframes, AAC, and one MPEG-TS muxer cut in memory into
// Plex's segments. Nothing touches the disk. Only cmd/video-worker links it.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/obinnaokechukwu/ffgo"

	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/gpu"
)

// Options are the pod's: its encoder and device, and how a job reaches its
// streamed inputs.
type Options struct {
	Encoder    string    // "h264_qsv", or "libx264" where no GPU is present
	Caps       *gpu.Caps // nil: decode and scale on the CPU
	RenderNode string
	Inputs     func(i int) io.Reader // a streamed input's bytes
	InputsDir  string                // the memory-backed emptyDir (burn-in)
	FontsDir   string                // the fonts text burn-in draws with (Noto Sans)
	// Skipped is told each packet or frame a decoder refused and the job
	// carried on past; nil tells nothing.
	Skipped func(err error)
}

func (o Options) onGPU() bool { return o.Caps != nil }

// Segment is one HLS segment, its bytes and the times its list line
// reports; NextStart is where the next segment, now being written, starts
// (nil on the last).
type Segment struct {
	video.SegmentTimes
	Data      []byte
	NextStart *time.Duration
}

// Sink takes a run's output, in order. A Sink that blocks holds the run:
// that is how a relay paces it.
type Sink interface {
	Segment(s Segment) error
	File(name string, data []byte) error // DASH
	Manifest(mpd []byte) error           // DASH
	Write(off int64, p []byte) error     // a whole file
	Progress(path, query string)
}

// Run converts job until its input ends, ctx ends or a step fails.
func Run(ctx context.Context, job video.Job, o Options, sink Sink) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case job.Output == video.OutputDASH && job.Audio == nil:
		return fmt.Errorf("%w: a DASH job without audio", video.ErrShape)
	case job.Output != video.OutputHLS && job.Output != video.OutputDASH:
		return fmt.Errorf("%w: output %s", video.ErrShape, job.Output)
	}
	in, err := openInput(ctx, job, o)
	if err != nil {
		return err
	}
	defer in.close()

	devs, err := openDevices(o)
	if err != nil {
		return err
	}
	defer devs.close()

	burn, err := newBurner(ctx, job, in, o)
	if err != nil {
		return err
	}
	if burn != nil {
		defer burn.close()
	}
	var c segmenter = newCutter(job, sink)
	if job.Output == video.OutputDASH {
		c = newDashCutter(job, sink, in.d.Duration())
	}
	defer c.close()
	vp, err := newVideoPath(job, in, o, devs, c, burn)
	if err != nil {
		return err
	}
	defer vp.close()
	vp.refused.skipped = o.Skipped
	var ap *audioPath
	if job.Audio != nil {
		if ap, err = newAudioPath(job, in, c); err != nil {
			return err
		}
		defer ap.close()
		ap.refused.skipped = o.Skipped
	} else {
		c.noAudio()
	}
	if burn != nil {
		burn.refused.skipped = o.Skipped
	}
	prog := newProgress(job, in.d.Duration(), vp.gpuDecode, sink)
	tl := newTimeline(in, job.Inputs[0].Streamed)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p, err := in.d.ReadPacket()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read: %w", err)
		}
		if p == nil {
			break
		}
		tl.fix(p)
		switch idx := p.StreamIndex(); {
		case idx == in.video.Index:
			err = vp.send(p)
		case ap != nil && idx == in.audio.Index:
			err = ap.send(p)
		case burn != nil && burn.sd != nil && idx == burn.index:
			err = burn.send(p)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		prog.at(c.videoTime())
	}
	if err := vp.flush(); err != nil {
		return err
	}
	if ap != nil {
		if err := ap.flush(); err != nil {
			return err
		}
	}
	if err := c.finish(); err != nil {
		return err
	}
	return ctx.Err()
}

// input is a job's opened input 0: its demuxer and the streams it maps.
type input struct {
	d            *ffgo.Decoder
	video, audio *ffgo.StreamInfo
	// shift is -start_at_zero's: the input's start time, taken off every
	// timestamp (-copyts keeps the rest).
	shift time.Duration
}

func (in *input) close() { _ = in.d.Close() }

// openInput opens input 0, a /media file or a streamed one, finds the
// streams the job maps and seeks to -ss.
func openInput(ctx context.Context, job video.Job, o Options) (*input, error) {
	if len(job.Inputs) == 0 {
		return nil, fmt.Errorf("%w: no input", video.ErrShape)
	}
	src := job.Inputs[0]
	opts := &ffgo.DecoderOptions{ProbeSizeBytes: 20_000_000, AnalyzeDuration: 20 * time.Second, Interrupt: ctx.Done()}
	var (
		d   *ffgo.Decoder
		err error
	)
	if src.Streamed {
		if o.Inputs == nil {
			return nil, errors.New("a streamed input without a stream")
		}
		opts.Format = src.Format
		d, err = ffgo.NewDecoderFromReaderWithOptions(o.Inputs(0), opts)
	} else {
		d, err = ffgo.NewDecoderWithOptions(src.Path, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("open the input: %w", err)
	}
	in := &input{d: d}
	for _, s := range d.Streams() {
		switch {
		case s.Index == job.Video.Stream:
			in.video = s
		case job.Audio != nil && s.Index == job.Audio.Stream:
			in.audio = s
		}
	}
	if in.video == nil || in.video.Type != ffgo.MediaTypeVideo {
		in.close()
		return nil, fmt.Errorf("%w: stream %d is no video stream", video.ErrShape, job.Video.Stream)
	}
	if job.Audio != nil && (in.audio == nil || in.audio.Type != ffgo.MediaTypeAudio) {
		in.close()
		return nil, fmt.Errorf("%w: stream %d is no audio stream", video.ErrShape, job.Audio.Stream)
	}
	if st := d.StartTime(); st > 0 {
		in.shift = st
	}
	if job.Start > 0 && !src.Streamed {
		// To the keyframe before -ss; the grid's first slot takes the frame
		// nearest -ss, and earlier frames only lead up to it.
		if err := d.Seek(job.Start + in.shift); err != nil {
			in.close()
			return nil, fmt.Errorf("seek to %s: %w", job.Start, err)
		}
	}
	return in, nil
}

// at is a timestamp of tb in source time: shifted by -start_at_zero.
func (in *input) at(ts int64, tb ffgo.Rational) time.Duration {
	return toTime(ts, tb) - in.shift
}

func toTime(v int64, tb ffgo.Rational) time.Duration {
	return time.Duration(float64(v) * float64(tb.Num) / float64(tb.Den) * float64(time.Second))
}

// devices are the pod's GPU, opened per run: VA-API to decode, QSV to
// scale and encode. Both nil where no GPU is.
type devices struct{ vaapi, qsv *ffgo.HWDevice }

func openDevices(o Options) (*devices, error) {
	if !o.onGPU() {
		return &devices{}, nil
	}
	if err := gpu.IsRenderNode(o.RenderNode); err != nil {
		return nil, err
	}
	vaapi, err := ffgo.NewHWDevice(ffgo.HWDeviceTypeVAAPI, o.RenderNode)
	if err != nil {
		return nil, fmt.Errorf("open %s for VA-API: %w", o.RenderNode, err)
	}
	qsv, err := ffgo.NewHWDevice(ffgo.HWDeviceTypeQSV, o.RenderNode)
	if err != nil {
		_ = vaapi.Close()
		return nil, fmt.Errorf("open %s for QSV: %w", o.RenderNode, err)
	}
	return &devices{vaapi: vaapi, qsv: qsv}, nil
}

func (d *devices) close() {
	if d.qsv != nil {
		_ = d.qsv.Close()
	}
	if d.vaapi != nil {
		_ = d.vaapi.Close()
	}
}
