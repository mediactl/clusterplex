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
