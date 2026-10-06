// Package pipeline is the remux worker's ffgo pipeline: one demux, the
// video copied, the audio converted, each representation muxed into
// fragmented MP4 through an io.Writer and cut into Plex's segments at
// deterministic keyframe boundaries. Only cmd/remux-worker links it.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/obinnaokechukwu/ffgo/avcodec"
	"github.com/obinnaokechukwu/ffgo/avutil"

	"github.com/mediactl/clusterplex/pkg/remux"
)

type Sink interface {
	Init(video, audio []byte, timescales [2]int32, info remux.StreamInfo) error
	Segment(s remux.Segment) error
	Progress(path, query string)
}

type Options struct {
	From    int
	StartAt time.Duration
}

// fragmented is how every representation is muxed: fragments cut on
// Flush, and timestamps left in source time as Plex's own jobs ask
// (-avoid_negative_ts disabled). The muxer's default shifts each run to
// start at 0, which would put a seek's first segment at the start of the
// timeline.
//
// use_editlist=0 keeps every fragment's decode times absolute, so a frame is
// presented at its own pts: with an edit list, frag_discont shifts the track
// by the first packet's pts-dts, which differs per run (82 ms from the
// start, 208 ms after a seek) and put video that far behind audio.
var fragmented = map[string]string{
	"movflags":          "frag_custom+empty_moov+default_base_moof+frag_discont",
	"avoid_negative_ts": "disabled",
	"use_editlist":      "0",
}

// PresentationOffset is added to every video and audio timestamp, in every
// run: a B-frame stream starting at 0 has decode times below 0, which
// absolute fragments cannot carry. The same for every file and run, so audio
// and video stay together and a segment's times do not depend on the run.
// It covers a reorder delay of up to 12 frames at 24 fps.
const PresentationOffset = 500 * time.Millisecond

// rep is one representation's muxer and what it has cut.
type rep struct {
	m     *ffgo.Muxer
	ms    *ffgo.MuxerStream
	split *remux.Splitter
	init  []byte
	frag  []byte // set by the splitter during Flush
	scale ffgo.Rational
	first int64 // first timestamp of the open fragment, output time base; -1 none
	end   int64 // end of the last packet written, output time base
}

func newRep() (*rep, error) {
	r := &rep{first: -1}
	r.split = &remux.Splitter{
		OnInit:     func(b []byte) error { r.init = append([]byte(nil), b...); return nil },
		OnFragment: func(b []byte) error { r.frag = append([]byte(nil), b...); return nil },
	}
	m, err := ffgo.NewMuxerToWriter(r.split, "mp4")
	if err != nil {
		return nil, err
	}
	r.m = m
	return r, nil
}

func (r *rep) header() error {
	if err := r.m.WriteHeaderWithOptions(fragmented); err != nil {
		return err
	}
	r.scale = r.ms.OutputTimeBase()
	return r.m.Flush() // the init reaches the splitter
}

// write muxes p, whose timestamps are in tb, and tracks the fragment's span.
func (r *rep) write(p *ffgo.Packet, tb ffgo.Rational) error {
	pts := rescale(p.PTS(), tb, r.scale)
	if r.first < 0 {
		r.first = pts
	}
	if end := pts + rescale(avcodec.GetPacketDuration(p.Raw()), tb, r.scale); end > r.end {
		r.end = end
	}
	return r.m.WritePacket(r.ms, p)
}

// cut ends the open fragment and returns it.
func (r *rep) cut() (remux.Fragment, error) {
	r.frag = nil
	if err := r.m.Flush(); err != nil {
		return remux.Fragment{}, err
	}
	f := remux.Fragment{Data: r.frag, T: r.first, D: r.end - r.first}
	r.first = -1
	return f, nil
}

func rescale(v int64, from, to ffgo.Rational) int64 {
	if v == avutil.AV_NOPTS_VALUE || from.Den == 0 || to.Num == 0 {
		return v
	}
	return v * int64(from.Num) * int64(to.Den) / (int64(from.Den) * int64(to.Num))
}

func toTime(v int64, tb ffgo.Rational) time.Duration {
	return time.Duration(v) * time.Second * time.Duration(tb.Num) / time.Duration(tb.Den)
}

var mpdCodecs = map[string]string{"hevc": "hev1", "h264": "avc1", "av1": "av01"}

// Run remuxes job from segment o.From to the end of the input, handing
// each finished segment to sink in order. It returns the last segment's
// number.
func Run(ctx context.Context, job remux.Job, o Options, sink Sink) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := ffgo.Init(); err != nil {
		return 0, err
	}
	d, err := ffgo.NewDecoder(job.Input)
	if err != nil {
		return 0, err
	}
	defer func() { _ = d.Close() }()
	var vsrc, asrc *ffgo.StreamInfo
	for _, s := range d.Streams() {
		switch s.Index {
		case job.VideoStream:
			vsrc = s
		case job.AudioStream:
			asrc = s
		}
	}
	if vsrc == nil || vsrc.Type != ffgo.MediaTypeVideo || asrc == nil || asrc.Type != ffgo.MediaTypeAudio {
		return 0, fmt.Errorf("%w: streams %d and %d are not video and audio", remux.ErrNotRemux, job.VideoStream, job.AudioStream)
	}
	codec, ok := mpdCodecs[vsrc.CodecName]
	if !ok {
		return 0, fmt.Errorf("%w: video codec %s", remux.ErrNotRemux, vsrc.CodecName)
	}
	shift := map[int]int64{}
	offset := map[int]int64{}
	for _, s := range []*ffgo.StreamInfo{vsrc, asrc} {
		offset[s.Index] = rescale(PresentationOffset.Microseconds(), ffgo.NewRational(1, 1000000), s.TimeBase)
	}
	if st := d.StartTime(); st > 0 {
		for _, s := range []*ffgo.StreamInfo{vsrc, asrc} {
			shift[s.Index] = rescale(st.Microseconds(), ffgo.NewRational(1, 1000000), s.TimeBase)
		}
	}

	video, err := newRep()
	if err != nil {
		return 0, err
	}
	defer func() { _ = video.m.Close() }()
	if video.ms, err = video.m.AddCopyStream(&ffgo.CopyStreamConfig{CodecParameters: vsrc.CodecParameters(), TimeBase: vsrc.TimeBase}); err != nil {
		return 0, err
	}
	if err := video.header(); err != nil {
		return 0, err
	}
	aud, err := newAudio(job, asrc, d, o.From)
	if err != nil {
		return 0, err
	}
	defer aud.close()

	channels, rate := job.AudioChannels, job.AudioSampleRate
	if channels == 0 {
		channels = asrc.Channels
	}
	if rate == 0 {
		rate = asrc.SampleRate
	}
	info := remux.StreamInfo{
		VideoCodec: codec, AudioCodec: "mp4a.40.2", Width: vsrc.Width, Height: vsrc.Height,
		FrameRate:  fmt.Sprintf("%d/%d", vsrc.FrameRate.Num, vsrc.FrameRate.Den),
		SampleRate: rate, Channels: channels, Duration: d.Duration(),
	}
	if err := sink.Init(video.init, aud.rep.init, [2]int32{video.scale.Den, aud.rep.scale.Den}, info); err != nil {
		return 0, err
	}
	announce(sink, vsrc, asrc, info)

	target := time.Duration(o.From-1) * job.SegmentDuration
	startAt := o.StartAt
	if startAt < 0 {
		startAt = target
	}
	if startAt > 0 {
		// To the keyframe before the boundary, not the one on it: Matroska
		// stores no DTS, and libavformat's guesses right after a seek can be
		// missing or step back a rounding (the mp4 muxer refuses that). One
		// keyframe interval read and dropped first gives the boundary's
		// packets the DTS a run from the start gives them.
		if err := d.Seek(startAt - time.Millisecond); err != nil {
			return 0, err
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b := newBoundaries()
	segs := newPairs(sink, o.From)
	var wg sync.WaitGroup
	var audioErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		if audioErr = aud.run(ctx, b, segs); audioErr != nil {
			cancel()
		}
	}()

	began := time.Now()
	n := o.From
	last := time.Duration(-1)
	grid := false
	var demuxErr error
	for {
		if demuxErr = ctx.Err(); demuxErr != nil {
			break
		}
		p, err := d.ReadPacket()
		if err != nil {
			demuxErr = err
			break
		}
		if p == nil {
			break
		}
		idx := p.StreamIndex()
		if idx != job.VideoStream && idx != job.AudioStream {
			continue
		}
		c, err := p.Clone()
		if err != nil {
			demuxErr = err
			break
		}
		if off := shift[idx]; off != 0 {
			if ts := c.PTS(); ts != avutil.AV_NOPTS_VALUE {
				avcodec.SetPacketPTS(c.Raw(), ts-off)
			}
			if ts := c.DTS(); ts != avutil.AV_NOPTS_VALUE {
				avcodec.SetPacketDTS(c.Raw(), ts-off)
			}
		}
		if idx == job.AudioStream {
			present(c, offset[idx])
			aud.queue.push(c)
			continue
		}
		ts := toTime(c.PTS(), vsrc.TimeBase)
		key := avcodec.GetPacketFlags(c.Raw())&avcodec.PacketFlagKey != 0
		switch {
		case last < 0: // dropping up to the first keyframe at or after startAt
			if !key || ts < startAt {
				_ = c.Free()
				continue
			}
			last = ts
			// A run's first segment is on the grid when the rule placed it;
			// one continuing a cache chain (an explicit StartAt) may not be.
			grid = o.StartAt < 0
			b.add(ts)
		case key && ts >= time.Duration(n)*job.SegmentDuration && ts > last:
			f, err := video.cut()
			if err != nil {
				demuxErr = err
			}
			segs.video(n, f, last, ts, grid)
			// Segment n+1 starts where a seek to it would only if segment
			// n began before n+1's grid point: otherwise the rule's "later
			// than the last start" pushed it past the grid's keyframe.
			grid = last < time.Duration(n)*job.SegmentDuration
			n++
			last = ts
			b.add(ts)
			progress(sink, ts, info.Duration, began)
			// Memory stays a few segments, not the file: the AAC encode is
			// slower than reading, and every undelivered fragment is held.
			if err := segs.waitRoom(ctx, n); err != nil && demuxErr == nil {
				demuxErr = err // the loop ends below, after this packet is freed
			}
		}
		if demuxErr == nil {
			present(c, offset[idx]) // after the boundary rule, which reads source time
			demuxErr = video.write(c, vsrc.TimeBase)
		}
		_ = c.Free()
		b.advance(ts)
		if demuxErr != nil {
			break
		}
	}
	aud.queue.close()
	if demuxErr == nil && last >= 0 {
		f, err := video.cut()
		if err != nil {
			demuxErr = err
		}
		segs.video(n, f, last, toTime(video.end, video.scale)-PresentationOffset, grid)
	}
	b.finish()
	wg.Wait()
	if err := errors.Join(demuxErr, audioErr, segs.err()); err != nil {
		if cerr := ctx.Err(); cerr != nil && errors.Is(err, cerr) {
			return 0, cerr
		}
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

// present moves a packet's timestamps by off, the presentation offset in
// its stream's time base.
func present(p *ffgo.Packet, off int64) {
	if ts := p.PTS(); ts != avutil.AV_NOPTS_VALUE {
		avcodec.SetPacketPTS(p.Raw(), ts+off)
	}
	if ts := p.DTS(); ts != avutil.AV_NOPTS_VALUE {
		avcodec.SetPacketDTS(p.Raw(), ts+off)
	}
}

// announce sends what Plex's binary sends before its first progress.
func announce(sink Sink, v, a *ffgo.StreamInfo, info remux.StreamInfo) {
	sink.Progress("stream", fmt.Sprintf("index=0&id=0&codec=%s&type=video", v.CodecName))
	sink.Progress("stream", fmt.Sprintf("index=1&id=0&codec=%s&type=audio", a.CodecName))
	sink.Progress("streamDetail", fmt.Sprintf("index=0&id=0&codec=%s&type=video&width=%d&height=%d", v.CodecName, v.Width, v.Height))
	sink.Progress("streamDetail", fmt.Sprintf("index=1&id=0&codec=%s&type=audio&channels=%d&sampleRate=%d", a.CodecName, a.Channels, a.SampleRate))
	sink.Progress("", fmt.Sprintf("duration=%f", info.Duration.Seconds()))
	sink.Progress("", fmt.Sprintf("width=%d&height=%d", info.Width, info.Height))
}

func progress(sink Sink, at, total time.Duration, began time.Time) {
	if total <= 0 {
		return
	}
	speed := at.Seconds() / max(time.Since(began).Seconds(), 0.001)
	remaining := (total - at).Seconds() / max(speed, 0.001)
	sink.Progress("", fmt.Sprintf("progress=%.1f&size=-1&remaining=%d&speed=%.1f", 100*at.Seconds()/total.Seconds(), int(remaining), speed))
}

// maxAhead is how many finished segments may wait for delivery before the
// demuxer waits too. With the demuxer paused at segment n's start, the
// audio side can still finish every segment up to n-2, so any value of 1
// or more cannot deadlock; 3 keeps a worker's memory to a few segments.
const maxAhead = 3

// pairs joins each segment's video and audio fragments and hands whole
// segments to the sink in order.
type pairs struct {
	mu      sync.Mutex
	sink    Sink
	next    int
	pending map[int]*remux.Segment
	have    map[int]int
	e       error

	// delivered is the next segment the sink has not yet returned from;
	// the demuxer waits on it (waitRoom).
	dmu       sync.Mutex
	dcond     *sync.Cond
	delivered int
}

func newPairs(sink Sink, from int) *pairs {
	p := &pairs{sink: sink, next: from, delivered: from, pending: map[int]*remux.Segment{}, have: map[int]int{}}
	p.dcond = sync.NewCond(&p.dmu)
	return p
}

// waitRoom blocks the demuxer, about to start segment n, while more than
// maxAhead finished segments wait for delivery, or until ctx ends.
func (p *pairs) waitRoom(ctx context.Context, n int) error {
	stop := context.AfterFunc(ctx, p.dcond.Broadcast)
	defer stop()
	p.dmu.Lock()
	defer p.dmu.Unlock()
	for n-p.delivered > maxAhead && ctx.Err() == nil {
		p.dcond.Wait()
	}
	return ctx.Err()
}

func (p *pairs) video(n int, f remux.Fragment, start, end time.Duration, onGrid bool) {
	p.put(n, func(s *remux.Segment) { s.Video, s.Start, s.End, s.OnGrid = f, start, end, onGrid })
}

func (p *pairs) audio(n int, f remux.Fragment) { p.put(n, func(s *remux.Segment) { s.Audio = f }) }

func (p *pairs) put(n int, set func(*remux.Segment)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.pending[n]
	if !ok {
		s = &remux.Segment{N: n}
		p.pending[n] = s
	}
	set(s)
	p.have[n]++
	for p.have[p.next] == 2 {
		s := p.pending[p.next]
		delete(p.pending, p.next)
		delete(p.have, p.next)
		p.next++
		if p.e == nil {
			p.e = p.sink.Segment(*s)
		}
		p.dmu.Lock()
		p.delivered = s.N + 1
		p.dmu.Unlock()
		p.dcond.Broadcast()
	}
}

func (p *pairs) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.e
}

// queue is the audio packets the demuxer read, unbounded so the demuxer
// never waits on the audio side (which waits on the demuxer's horizon).
type queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []*ffgo.Packet
	closed bool
}

func newQueue() *queue {
	q := &queue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *queue) push(p *ffgo.Packet) {
	q.mu.Lock()
	q.items = append(q.items, p)
	q.mu.Unlock()
	q.cond.Signal()
}

func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// pop returns the next packet, nil once closed and drained or ctx ends.
func (q *queue) pop(ctx context.Context) *ffgo.Packet {
	stop := context.AfterFunc(ctx, q.cond.Broadcast)
	defer stop()
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed && ctx.Err() == nil {
		q.cond.Wait()
	}
	if len(q.items) == 0 || ctx.Err() != nil {
		return nil
	}
	p := q.items[0]
	q.items = q.items[1:]
	return p
}

func (q *queue) drain() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, p := range q.items {
		_ = p.Free()
	}
	q.items = nil
}

// audio decodes, resamples and encodes (or copies) the audio stream into
// its representation, cutting at the video's boundaries.
type audio struct {
	rep   *rep
	queue *queue
	src   *ffgo.StreamInfo
	copy  bool
	sd    *ffgo.StreamDecoder
	enc   *ffgo.AudioEncoder
	res   *ffgo.Resampler
	rate  int
	lay   string
	from  int
}

// newAudio starts numbering at from, the run's first segment: a run that
// fills a cache gap starts past the job's own first segment.
func newAudio(job remux.Job, src *ffgo.StreamInfo, d *ffgo.Decoder, from int) (*audio, error) {
	r, err := newRep()
	if err != nil {
		return nil, err
	}
	a := &audio{rep: r, queue: newQueue(), src: src, copy: job.AudioCopy, from: from}
	if a.copy {
		if r.ms, err = r.m.AddCopyStream(&ffgo.CopyStreamConfig{
			CodecParameters: src.CodecParameters(), TimeBase: src.TimeBase,
			Options: ffgo.StreamOptions{Language: job.AudioLanguage},
		}); err != nil {
			return nil, err
		}
		return a, r.header()
	}
	a.rate = job.AudioSampleRate
	if a.rate == 0 {
		a.rate = src.SampleRate
	}
	a.lay = "stereo"
	if job.AudioChannels != 2 {
		a.lay = src.ChannelLayout
	}
	if a.sd, err = d.NewStreamDecoder(src.Index, nil); err != nil {
		return nil, err
	}
	if a.enc, err = ffgo.NewAudioEncoder(ffgo.AudioEncoderConfig2{
		SampleRate: a.rate, Layout: a.lay, BitRate: job.AudioBitRate,
		GlobalHeader: true, InputTimeBase: a.sd.TimeBase(),
	}); err != nil {
		return nil, err
	}
	if r.ms, err = r.m.AddEncoderStream(a.enc, ffgo.StreamOptions{Language: job.AudioLanguage}); err != nil {
		return nil, err
	}
	return a, r.header()
}

func (a *audio) close() {
	a.queue.drain()
	if a.res != nil {
		_ = a.res.Close()
	}
	if a.enc != nil {
		_ = a.enc.Close()
	}
	if a.sd != nil {
		_ = a.sd.Close()
	}
	_ = a.rep.m.Close()
}

// run consumes the queue until the demuxer closes it.
func (a *audio) run(ctx context.Context, b *boundaries, segs *pairs) error {
	n := a.from
	tb := a.src.TimeBase
	if !a.copy {
		tb = a.enc.TimeBase()
	}
	emit := func(p *ffgo.Packet) error {
		t := toTime(p.PTS(), tb) - PresentationOffset // the boundaries are source time
		starts, err := b.wait(ctx, t)
		if err != nil {
			return err
		}
		if len(starts) == 0 || t < starts[0] {
			return nil // before the first segment's keyframe
		}
		for i := n - a.from + 1; i < len(starts) && t >= starts[i]; i = n - a.from + 1 {
			f, err := a.rep.cut()
			if err != nil {
				return err
			}
			segs.audio(n, f)
			n++
		}
		return a.rep.write(p, tb)
	}
	encode := func(p *ffgo.Packet) error {
		if a.copy {
			return emit(p)
		}
		return a.decode(p, emit)
	}
	for {
		p := a.queue.pop(ctx)
		if p == nil {
			break
		}
		err := encode(p)
		_ = p.Free()
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !a.copy {
		if err := a.decode(nil, emit); err != nil {
			return err
		}
		if a.res != nil {
			if tail, err := a.res.Flush(); err == nil && !tail.IsNil() {
				err := a.enc.Encode(tail, emit)
				_ = tail.Free()
				if err != nil {
					return err
				}
			}
		}
		if err := a.enc.Flush(emit); err != nil {
			return err
		}
	}
	f, err := a.rep.cut()
	if err != nil {
		return err
	}
	segs.audio(n, f)
	return nil
}

// decode sends p (nil at the end) and encodes every frame it yields.
func (a *audio) decode(p *ffgo.Packet, emit func(*ffgo.Packet) error) error {
	for {
		err := a.sd.Send(p)
		if err != nil && !errors.Is(err, ffgo.ErrAgain) {
			return err
		}
		for {
			f, rerr := a.sd.Receive()
			if errors.Is(rerr, ffgo.ErrAgain) || errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				return rerr
			}
			if a.res == nil {
				if a.res, rerr = ffgo.NewResampler(
					ffgo.AudioFormat{SampleRate: a.src.SampleRate, Layout: f.ChannelLayout(), SampleFormat: ffgo.SampleFormat(f.Format())},
					ffgo.AudioFormat{SampleRate: a.rate, Layout: a.lay, SampleFormat: ffgo.SampleFormatFLTP}); rerr != nil {
					return rerr
				}
			}
			r, rerr := a.res.Resample(f)
			if rerr != nil {
				return rerr
			}
			if r.IsNil() {
				continue
			}
			r.SetPTS(f.PTS())
			rerr = a.enc.Encode(r, emit)
			_ = r.Free()
			if rerr != nil {
				return rerr
			}
		}
		if !errors.Is(err, ffgo.ErrAgain) {
			return nil
		}
	}
}
