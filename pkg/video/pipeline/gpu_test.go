package pipeline_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/pkg/video/gpu"
	"github.com/mediactl/clusterplex/pkg/video/pipeline"
)

// paced is a clip read no faster than real time, as a live channel arrives.
type paced struct {
	r     *bytes.Reader
	rate  float64 // bytes per second
	began time.Time
	read  int
}

func (p *paced) Read(b []byte) (int, error) {
	if due := time.Duration(float64(p.read) / p.rate * float64(time.Second)); time.Since(p.began) < due {
		time.Sleep(due - time.Since(p.began))
	}
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	n, err := p.r.Read(b)
	p.read += n
	return n, err
}

// packets reads an MPEG-TS's video packets: their times and sizes.
func packets(t *testing.T, ts []byte) (times []float64, bits []float64) {
	t.Helper()
	d, err := ffgo.NewDecoderFromReader(bytes.NewReader(ts), "mpegts")
	require.NoError(t, err)
	defer func() { _ = d.Close() }()
	v := d.VideoStream()
	require.NotNil(t, v)
	for {
		p, err := d.ReadPacket()
		require.NoError(t, err)
		if p == nil {
			return times, bits
		}
		if p.StreamIndex() == v.Index {
			times = append(times, float64(p.PTS())*float64(v.TimeBase.Num)/float64(v.TimeBase.Den))
			bits = append(bits, float64(p.Size()*8))
		}
	}
}

// TestTheQSVPipelineMeetsTheStartupTarget runs where an Intel render node
// is expected (CLUSTERPLEX_EXPECT_INTEL=1, the video-test image on the NUC)
// and fails there rather than skip.
func TestTheQSVPipelineMeetsTheStartupTarget(t *testing.T) {
	if os.Getenv("CLUSTERPLEX_EXPECT_INTEL") != "1" {
		t.Skip("no Intel render node expected")
	}
	const node = "/dev/dri/renderD128"
	_, err := os.Stat(node)
	require.NoError(t, err, "the Intel render node")
	caps, err := gpu.Measure(t.Context(), node)
	require.NoError(t, err)
	src, err := gpu.Clip(t.Context(), "h264", 8, 60*time.Second, 1920, 1080)
	require.NoError(t, err)

	job := library(t, "")
	job.Inputs[0] = video.Input{Streamed: true, Format: "matroska"}
	job.Audio = nil
	o := pipeline.Options{Encoder: "h264_qsv", Caps: &caps, RenderNode: node}

	t.Run("live", func(t *testing.T) {
		o := o
		began := time.Now()
		o.Inputs = func(int) io.Reader {
			return &paced{r: bytes.NewReader(src), rate: float64(len(src)) / 60, began: began}
		}
		first := make(chan time.Duration, 1)
		r := &recorder{onSeg: func(pipeline.Segment) {
			select {
			case first <- time.Since(began):
			default:
			}
		}}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		require.NoError(t, pipeline.Run(ctx, job, o, r))
		overhead := <-first - 8008*time.Millisecond
		t.Logf("start-up overhead %s", overhead)
		require.LessOrEqual(t, overhead, 2*time.Second, "the owner's target: about 2 s beyond the first segment's input")
	})

	t.Run("file", func(t *testing.T) {
		o := o
		o.Inputs = func(int) io.Reader { return bytes.NewReader(src) }
		r := &recorder{}
		began := time.Now()
		require.NoError(t, pipeline.Run(t.Context(), job, o, r))
		speed := 60 / time.Since(began).Seconds()
		t.Logf("encode speed %.1fx", speed)
		require.GreaterOrEqual(t, speed, 5.0)

		var all []byte
		for _, s := range r.segments {
			all = append(all, s.Data...)
		}
		times, bits := packets(t, all)
		window := 2 * float64(job.Video.BufSize) / float64(job.Video.MaxRate)
		limit := 1.1 * float64(job.Video.MaxRate) * window
		for i := range times {
			sum := 0.0
			for j := i; j < len(times) && times[j] < times[i]+window; j++ {
				sum += bits[j]
			}
			require.LessOrEqual(t, sum, limit, "%.0f bits in the %.1f s from %.3f s", sum, window, times[i])
		}
	})
}

// The GPU image carries no FFmpeg CLI, so its clips are made ahead, by
//
//	ffmpeg -f lavfi -i testsrc2=size=640x360:rate=24 -t 2 -pix_fmt yuv420p10le -c:v libx265 \
//	  -x265-params keyint=48:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc \
//	  -color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc hdr10.mkv
//	ffmpeg -f lavfi -i testsrc2=size=720x480:rate=60000/1001 -vf tinterlace=mode=interleave_top,setfield=tff \
//	  -t 2 -c:v mpeg2video -flags +ildct+ilme -b:v 1500k -an interlaced.ts
const (
	hdr10Clip    = "../testdata/clips/hdr10.mkv"
	interlacedTS = "../testdata/clips/interlaced.ts"
)

// TestTheQSVPipelineToneMapsAndDeinterlaces runs where an Intel render
// node is expected: an HDR10 film comes out BT.709 SDR through vpp_qsv's
// tone map, and an interlaced channel converts through its deinterlacer
// (the CPU suite measures the weave gone; here the device runs it).
func TestTheQSVPipelineToneMapsAndDeinterlaces(t *testing.T) {
	if os.Getenv("CLUSTERPLEX_EXPECT_INTEL") != "1" {
		t.Skip("no Intel render node expected")
	}
	const node = "/dev/dri/renderD128"
	caps, err := gpu.Measure(t.Context(), node)
	require.NoError(t, err)
	require.True(t, caps.ToneMap, "the device tone-maps")
	require.True(t, caps.Deinterlace, "the device deinterlaces")
	o := pipeline.Options{Encoder: "h264_qsv", Caps: &caps, RenderNode: node}
	run := func(t *testing.T, path string, w, h int, rate video.Rational) []byte {
		job := library(t, path)
		job.Audio = nil
		job.Video.Width, job.Video.Height, job.Video.FrameRate = w, h, rate
		r := &recorder{}
		require.NoError(t, pipeline.Run(t.Context(), job, o, r))
		var all []byte
		for _, s := range r.segments {
			all = append(all, s.Data...)
		}
		require.NotEmpty(t, all)
		return all
	}

	t.Run("hdr10", func(t *testing.T) {
		out := run(t, hdr10Clip, 640, 360, video.Rational{Num: 24, Den: 1})
		d, err := ffgo.NewDecoderFromReader(bytes.NewReader(out), "mpegts")
		require.NoError(t, err)
		defer func() { _ = d.Close() }()
		v := d.VideoStream()
		require.NotNil(t, v)
		require.Equal(t, "yuv420p", ffgo.PixelFormatName(v.PixelFmt))
		require.Equal(t, ffgo.ColorPrimariesBT709, v.Color.Primaries)
		require.Equal(t, ffgo.ColorTransferBT709, v.Color.Transfer)
		require.Equal(t, ffgo.ColorSpaceBT709, v.Color.Space)
	})

	t.Run("interlaced", func(t *testing.T) {
		times, _ := packets(t, run(t, interlacedTS, 720, 480, video.Rational{Num: 30000, Den: 1001}))
		require.InDelta(t, 60, len(times), 2, "one frame per woven frame, at the source's rate")
	})
}

// burnClip is testdata/clips/burn.mkv (TestWriteTheBurnClip).
const burnClip = "../testdata/clips/burn.mkv"

// planes decodes frame n of a converted MPEG-TS with ffgo (the GPU image
// has no FFmpeg CLI) and copies its yuv420p planes.
func planes(t *testing.T, ts []byte, n int) (y, u, v []byte, w, h int) {
	t.Helper()
	d, err := ffgo.NewDecoderFromReader(bytes.NewReader(ts), "mpegts")
	require.NoError(t, err)
	defer func() { _ = d.Close() }()
	vs := d.VideoStream()
	sd, err := d.NewStreamDecoder(vs.Index, &ffgo.StreamDecoderConfig{})
	require.NoError(t, err)
	defer func() { _ = sd.Close() }()
	seen := 0
	grab := func(f ffgo.Frame) bool {
		defer func() { seen++ }()
		if seen != n {
			return false
		}
		fw := ffgo.WrapFrame(f, ffgo.MediaTypeVideo)
		w, h = fw.Width(), fw.Height()
		row := func(p, stride, rw, rh int) []byte {
			out := make([]byte, 0, rw*rh)
			data := fw.Data(p)
			for r := range rh {
				out = append(out, data[r*stride:r*stride+rw]...)
			}
			return out
		}
		y, u, v = row(0, fw.Linesize(0), w, h), row(1, fw.Linesize(1), w/2, h/2), row(2, fw.Linesize(2), w/2, h/2)
		return true
	}
	drain := func() bool {
		for {
			f, err := sd.Receive()
			if err != nil {
				return false
			}
			got := grab(f)
			_ = f.Free()
			if got {
				return true
			}
		}
	}
	for {
		p, err := d.ReadPacket()
		require.NoError(t, err)
		if p == nil {
			_ = sd.Send(nil)
			require.True(t, drain(), "no frame %d", n)
			return
		}
		if p.StreamIndex() != vs.Index {
			continue
		}
		require.NoError(t, sd.Send(p))
		if drain() {
			return
		}
	}
}

// TestTheQSVPipelineBurnsSubtitlesIn runs where an Intel render node is
// expected: text through libass with the image's own Noto Sans and
// fontconfig, and a PGS box composited, before the upload to h264_qsv.
func TestTheQSVPipelineBurnsSubtitlesIn(t *testing.T) {
	if os.Getenv("CLUSTERPLEX_EXPECT_INTEL") != "1" {
		t.Skip("no Intel render node expected")
	}
	const node = "/dev/dri/renderD128"
	caps, err := gpu.Measure(t.Context(), node)
	require.NoError(t, err)
	o := pipeline.Options{Encoder: "h264_qsv", Caps: &caps, RenderNode: node, InputsDir: t.TempDir(), FontsDir: "/usr/share/fonts/noto"}
	run := func(t *testing.T, burn *video.BurnIn) []byte {
		job := library(t, burnClip)
		job.Audio = nil
		job.Burn = burn
		job.Video.Width, job.Video.Height, job.Video.FrameRate = 640, 360, video.Rational{Num: 24, Den: 1}
		r := &recorder{}
		require.NoError(t, pipeline.Run(t.Context(), job, o, r))
		var all []byte
		for _, s := range r.segments {
			all = append(all, s.Data...)
		}
		return all
	}
	plain := run(t, nil)
	py, pu, pv, w, h := planes(t, plain, 48)

	t.Run("text", func(t *testing.T) {
		y, _, _, _, _ := planes(t, run(t, &video.BurnIn{Input: 0, Stream: 1}), 48)
		changed := 0
		for i := w * h * 4 / 5; i < w*h; i++ {
			if d := int(y[i]) - int(py[i]); d > 32 || d < -32 {
				changed++
			}
		}
		require.Greater(t, changed, w*h/5/200, "HELLO is drawn at 2 s (%d luma pixels changed)", changed)
	})

	t.Run("image", func(t *testing.T) {
		_, u, v, _, _ := planes(t, run(t, &video.BurnIn{Input: 0, Stream: 2, Image: true}), 48)
		// The box's centre, (132, 516) on 1280x720, scaled to 640x360.
		i := (258/2)*(w/2) + 66/2
		require.Greater(t, int(v[i]), int(u[i])+40, "red at 2 s: U %d V %d", u[i], v[i])
		j := (258/2)*(w/2) + (66+80)/2
		require.InDelta(t, int(pu[j]), int(u[j]), 4, "beside the box, untouched")
		require.InDelta(t, int(pv[j]), int(v[j]), 4, "beside the box, untouched")
	})
}
