package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/obinnaokechukwu/ffgo/avcodec"

	"github.com/mediactl/clusterplex/pkg/video"
)

// muxOptions are Plex's: timestamps kept as they are (-copyts,
// -avoid_negative_ts disabled) and its -max_delay.
var muxOptions = map[string]string{"mpegts_copyts": "1", "max_delay": "5000000", "avoid_negative_ts": "disabled"}

// maxQueued bounds how many packets of one stream wait for the other's to
// interleave with: an audio gap, or a video encoder still filling, does
// not hold the stream forever.
const maxQueued = 512

// cutter is one MPEG-TS muxer for the whole job, written into memory, and
// cut into Plex's segments: packets of both streams in decode order, and
// before a forced keyframe the rule says starts the next segment, every
// byte so far drained into the segment that ends there (findings L, R).
type cutter struct {
	job  video.Job
	sink Sink
	buf  bytes.Buffer
	m    *ffgo.Muxer
	vs   *ffgo.MuxerStream
	as   *ffgo.MuxerStream

	venc      *ffgo.VideoStreamEncoder
	aenc      *ffgo.AudioEncoder
	acopy     *ffgo.StreamInfo
	vtb, atb  ffgo.Rational
	wantAudio bool

	vq, aq []queued

	n                int // the open segment's number
	open             bool
	start            time.Duration
	vEnd, aEnd, last time.Duration
}

type queued struct {
	p        *ffgo.Packet
	dts, pts time.Duration
	end      time.Duration
	key      bool
}

func newCutter(job video.Job, sink Sink) *cutter {
	return &cutter{job: job, sink: sink, n: job.Segments.StartNumber, wantAudio: job.Audio != nil}
}

func (c *cutter) noAudio() { c.wantAudio = false }

func (c *cutter) openAudio(enc *ffgo.AudioEncoder) { c.aenc, c.atb = enc, enc.TimeBase() }

func (c *cutter) openAudioCopy(si *ffgo.StreamInfo) { c.acopy, c.atb = si, si.TimeBase }

// openVideo starts the muxer once the video encoder exists: its streams
// need both encoders' parameters.
func (c *cutter) openVideo(enc *ffgo.VideoStreamEncoder) error {
	c.venc, c.vtb = enc, enc.TimeBase()
	m, err := ffgo.NewMuxerToWriter(&c.buf, "mpegts")
	if err != nil {
		return err
	}
	c.m = m
	if c.vs, err = m.AddEncoderStream(enc, ffgo.StreamOptions{}); err != nil {
		return err
	}
	lang := ffgo.StreamOptions{}
	if c.job.Audio != nil {
		lang.Language = c.job.Audio.Language
	}
	switch {
	case c.aenc != nil:
		c.as, err = m.AddEncoderStream(c.aenc, lang)
	case c.acopy != nil:
		c.as, err = m.AddCopyStream(&ffgo.CopyStreamConfig{CodecParameters: c.acopy.CodecParameters(), TimeBase: c.acopy.TimeBase, Options: lang})
	}
	if err != nil {
		return err
	}
	if err := m.WriteHeaderWithOptions(muxOptions); err != nil {
		return fmt.Errorf("mpegts header: %w", err)
	}
	return c.pump(false)
}

func (c *cutter) close() {
	for _, q := range slices.Concat(c.vq, c.aq) {
		_ = q.p.Free()
	}
	c.vq, c.aq = nil, nil
	if c.m != nil {
		_ = c.m.Close()
	}
}

func (c *cutter) frame() time.Duration {
	r := c.job.Video.FrameRate
	return time.Duration(int64(time.Second) * int64(r.Den) / int64(r.Num))
}

// video takes an encoded video packet (valid only during the call).
func (c *cutter) video(p *ffgo.Packet) error {
	q, err := c.queue(p, c.vtb, c.frame())
	if err != nil {
		return err
	}
	q.key = avcodec.GetPacketFlags(p.Raw())&avcodec.PacketFlagKey != 0
	c.vq = append(c.vq, q)
	c.last = q.pts
	return c.pump(false)
}

// audio takes an audio packet (valid only during the call).
func (c *cutter) audio(p *ffgo.Packet) error {
	q, err := c.queue(p, c.atb, 0)
	if err != nil {
		return err
	}
	c.aq = append(c.aq, q)
	return c.pump(false)
}

func (c *cutter) queue(p *ffgo.Packet, tb ffgo.Rational, frame time.Duration) (queued, error) {
	cl, err := p.Clone()
	if err != nil {
		return queued{}, err
	}
	dts := cl.DTS()
	if dts < 0 && cl.PTS() >= 0 {
		dts = cl.PTS()
	}
	return queued{p: cl, dts: toTime(dts, tb), pts: toTime(cl.PTS(), tb), end: packetEnd(cl, tb, frame)}, nil
}

// videoTime is the latest video time encoded, for progress.
func (c *cutter) videoTime() time.Duration { return c.last }

// pump writes queued packets in decode order across the streams, as far
// as both streams have reached (all of them when final).
func (c *cutter) pump(final bool) error {
	if c.m == nil {
		return nil
	}
	for {
		var video bool
		switch {
		case len(c.vq) > 0 && len(c.aq) > 0:
			video = c.vq[0].dts <= c.aq[0].dts
		case len(c.vq) > 0 && (final || !c.wantAudio || len(c.vq) > maxQueued):
			video = true
		case len(c.aq) > 0 && (final || len(c.aq) > maxQueued):
			video = false
		default:
			return nil
		}
		var q queued
		if video {
			q, c.vq = c.vq[0], c.vq[1:]
		} else {
			q, c.aq = c.aq[0], c.aq[1:]
		}
		err := c.write(q, video)
		_ = q.p.Free()
		if err != nil {
			return err
		}
	}
}

func (c *cutter) write(q queued, video bool) error {
	if video && q.key && (!c.open || videoCutDue(q.pts, c.n, c.job.Segments)) {
		if c.open {
			if err := c.m.Drain(); err != nil {
				return fmt.Errorf("drain: %w", err)
			}
			next := q.pts
			if err := c.cut(next, &next); err != nil {
				return err
			}
		}
		c.open, c.start, c.vEnd, c.aEnd = true, q.pts, q.pts, q.pts
	}
	ms := c.vs
	if !video {
		ms = c.as
	}
	if ms == nil {
		return nil
	}
	if err := c.m.WritePacket(ms, q.p); err != nil {
		return fmt.Errorf("mux: %w", err)
	}
	if video {
		c.vEnd = max(c.vEnd, q.end)
	} else {
		c.aEnd = max(c.aEnd, q.end)
	}
	return nil
}

func videoCutDue(pts time.Duration, n int, s video.Segmenting) bool {
	return video.CutDue(pts, 0, n, s)
}

// cut hands the open segment, ending at end, to the sink: every byte the
// muxer has written since the last cut, which the caller drained (or the
// trailer flushed); next is where the next segment starts (nil for the
// last).
func (c *cutter) cut(end time.Duration, next *time.Duration) error {
	data := bytes.Clone(c.buf.Bytes())
	c.buf.Reset()
	st := video.SegmentTimes{
		Name: video.SegmentName(c.job.Segments.Pattern, c.n), Start: c.start, End: end,
		Streams: []video.Span{{Stream: 0, Start: c.start, End: end}},
	}
	if c.as != nil {
		st.Streams = append(st.Streams, video.Span{Stream: 1, Start: c.start, End: c.aEnd})
	}
	c.n++
	return c.sink.Segment(Segment{SegmentTimes: st, Data: data, NextStart: next})
}

// finish writes what is queued, the trailer, and the last segment.
func (c *cutter) finish() error {
	if c.m == nil {
		return errors.New("no video was encoded")
	}
	if err := c.pump(true); err != nil {
		return err
	}
	// The trailer flushes the muxer to the writer; nothing may be written,
	// or drained, after it.
	if err := c.m.WriteTrailer(); err != nil {
		return fmt.Errorf("mpegts trailer: %w", err)
	}
	if !c.open {
		return errors.New("no segment was written")
	}
	return c.cut(c.vEnd, nil)
}
