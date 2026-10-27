package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/obinnaokechukwu/ffgo/avcodec"
	"github.com/obinnaokechukwu/ffgo/avformat"
	"github.com/obinnaokechukwu/ffgo/avutil"

	"github.com/mediactl/clusterplex/pkg/remux"
	"github.com/mediactl/clusterplex/pkg/video"
)

// segmenter is what the video and audio paths hand their packets to: the
// MPEG-TS cutter of an HLS job, or the DASH cutter.
type segmenter interface {
	noAudio()
	openAudio(enc *ffgo.AudioEncoder)
	openAudioCopy(si *ffgo.StreamInfo)
	openVideo(enc *ffgo.VideoStreamEncoder) error
	video(p *ffgo.Packet) error
	audio(p *ffgo.Packet) error
	videoTime() time.Duration
	globalHeader() bool
	finish() error
	close()
}

func (c *cutter) globalHeader() bool { return false }

// fragmented is the remux pool's fMP4 (pkg/remux/pipeline, copied: it is
// unexported there): fragments cut on Flush, timestamps left in source time
// as Plex's jobs ask, and no edit list, so every frame is presented at its
// own pts.
var fragmented = map[string]string{
	"movflags":          "frag_custom+empty_moov+default_base_moof+frag_discont",
	"avoid_negative_ts": "disabled",
	"use_editlist":      "0",
}

// dashRep is one representation's fMP4 muxer, written into memory and cut
// into its init and fragments by the remux pool's splitter.
type dashRep struct {
	m     *ffgo.Muxer
	ms    *ffgo.MuxerStream
	split *remux.Splitter
	init  []byte
	frag  []byte // set by the splitter during Flush
	tb    ffgo.Rational
	scale ffgo.Rational
	first int64 // the open fragment's first timestamp in scale; -1 none
	end   int64
}

func newDashRep() (*dashRep, error) {
	r := &dashRep{first: -1}
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

func (r *dashRep) header(tb ffgo.Rational) error {
	r.tb = tb
	if err := r.m.WriteHeaderWithOptions(fragmented); err != nil {
		return fmt.Errorf("fmp4 header: %w", err)
	}
	r.scale = r.ms.OutputTimeBase()
	return r.m.Flush() // the init reaches the splitter
}

func (r *dashRep) write(p *ffgo.Packet, end time.Duration) error {
	pts := dashRescale(p.PTS(), r.tb, r.scale)
	if r.first < 0 {
		r.first = pts
	}
	if e := int64(end) * int64(r.scale.Den) / (int64(r.scale.Num) * int64(time.Second)); e > r.end {
		r.end = e
	}
	return r.m.WritePacket(r.ms, p)
}

func (r *dashRep) cut() (remux.Fragment, error) {
	r.frag = nil
	if err := r.m.Flush(); err != nil {
		return remux.Fragment{}, err
	}
	f := remux.Fragment{Data: r.frag, T: r.first, D: r.end - r.first}
	r.first = -1
	return f, nil
}

func dashRescale(v int64, from, to ffgo.Rational) int64 {
	if v == avutil.AV_NOPTS_VALUE || from.Den == 0 || to.Num == 0 {
		return v
	}
	return v * int64(from.Num) * int64(to.Den) / (int64(from.Den) * int64(to.Num))
}

// dashCutter is Plex Web's converted DASH (finding W) as the remux pool
// serves it: the video and the audio each an fMP4 representation, cut at
// the forced keyframe that starts segment n (numbered from
// -skip_to_segment, segment n ending at n segment durations), each
// segment's files sent before the manifest that lists them, and a static
// manifest at the end.
type dashCutter struct {
	job      video.Job
	sink     Sink
	duration time.Duration
	// live is a streamed input (Live TV), whose manifest lists the last
	// -window_size segments as FFmpeg's does; a file's lists every one
	// (Task 13).
	live bool
	now  func() time.Time

	v, a  *dashRep
	venc  *ffgo.VideoStreamEncoder
	aenc  *ffgo.AudioEncoder
	acopy *ffgo.StreamInfo
	atb   ffgo.Rational
	// adts is copied AAC that names no AudioSpecificConfig (Live TV's ADTS
	// in MPEG-TS): asc is the one its first header describes, and every
	// packet loses its header, as aac_adtstoasc does.
	adts bool
	asc  []byte

	aq       []queued // audio past the open segment's earliest end, until the video cuts
	n        int
	open     bool
	last     time.Duration
	sentInit bool
	manifest remux.Manifest
}

func newDashCutter(job video.Job, sink Sink, duration time.Duration) *dashCutter {
	live := len(job.Inputs) > 0 && job.Inputs[0].Streamed
	return &dashCutter{job: job, sink: sink, duration: duration, live: live, now: time.Now, n: job.Segments.StartNumber}
}

func (c *dashCutter) noAudio()                         {}
func (c *dashCutter) globalHeader() bool               { return true }
func (c *dashCutter) videoTime() time.Duration         { return c.last }
func (c *dashCutter) openAudio(enc *ffgo.AudioEncoder) { c.aenc, c.atb = enc, enc.TimeBase() }
func (c *dashCutter) openAudioCopy(si *ffgo.StreamInfo) {
	c.acopy, c.atb = si, si.TimeBase
	c.adts = si.Codec == "aac" && len(avformat.GetCodecParExtradata(si.CodecParameters())) == 0
}

// openVideo starts the video representation once the video encoder
// exists, and the audio one beside it -- but for copied AAC that names no
// AudioSpecificConfig (Live TV's ADTS in MPEG-TS), which opens at its first
// packet: an fMP4's init carries the config, and only the ADTS header says
// what it is.
func (c *dashCutter) openVideo(enc *ffgo.VideoStreamEncoder) error {
	c.venc = enc
	var err error
	if c.v, err = newDashRep(); err != nil {
		return err
	}
	if c.v.ms, err = c.v.m.AddEncoderStream(enc, ffgo.StreamOptions{}); err != nil {
		return err
	}
	if err := c.v.header(enc.TimeBase()); err != nil {
		return err
	}
	v := c.job.Video
	c.manifest = remux.Manifest{
		Duration: c.duration, StartNumber: c.job.Segments.StartNumber, SegmentDuration: c.job.Segments.Duration, Start: c.now(),
		Video: remux.Representation{
			Codecs: avcCodecs(c.v.init), Bandwidth: v.MaxRate, Width: v.Width, Height: v.Height,
			FrameRate: fmt.Sprintf("%d/%d", v.FrameRate.Num, v.FrameRate.Den), Timescale: c.v.scale.Den,
		},
	}
	if c.acopy != nil {
		switch {
		case c.acopy.Codec != "aac":
			// The mp4 muxer writes an AC-3 or E-AC-3 init only from a packet,
			// after the moov it must write first.
			return fmt.Errorf("%w: a DASH copy of %s audio", video.ErrShape, c.acopy.Codec)
		case c.adts:
			return nil // audio() opens it at its first packet
		}
	}
	return c.openAudioRep(nil)
}

// openAudioRep starts the audio representation; asc, when set, is a copied
// AAC stream's AudioSpecificConfig.
func (c *dashCutter) openAudioRep(asc []byte) error {
	var err error
	if c.a, err = newDashRep(); err != nil {
		return err
	}
	a := *c.job.Audio
	lang := ffgo.StreamOptions{Language: a.Language}
	codecs := "mp4a.40.2"
	switch {
	case c.aenc != nil:
		c.a.ms, err = c.a.m.AddEncoderStream(c.aenc, lang)
	case c.acopy != nil:
		// The source's parameters less its container's codec tag (MPEG-TS's
		// is no mp4 tag, which the muxer refuses), as ffmpeg(1) copies.
		par := avcodec.ParametersAlloc()
		defer avcodec.ParametersFree(&par)
		if err := avcodec.ParametersCopy(par, c.acopy.CodecParameters()); err != nil {
			return err
		}
		avcodec.SetCodecParTag(par, 0)
		if asc != nil {
			avformat.SetCodecParExtradata(par, asc)
			codecs = fmt.Sprintf("mp4a.40.%d", asc[0]>>3)
		}
		c.a.ms, err = c.a.m.AddCopyStream(&ffgo.CopyStreamConfig{CodecParameters: par, TimeBase: c.acopy.TimeBase, Options: lang})
		a.BitRate, a.SampleRate, a.Channels = c.acopy.BitRate, c.acopy.SampleRate, c.acopy.Channels
	default:
		err = errors.New("a DASH job without audio")
	}
	if err != nil {
		return err
	}
	if err := c.a.header(c.atb); err != nil {
		return err
	}
	c.manifest.Audio = remux.Representation{
		Codecs: codecs, Bandwidth: a.BitRate, SampleRate: a.SampleRate, Channels: a.Channels,
		Language: a.Language, Timescale: c.a.scale.Den,
	}
	return nil
}

// withoutADTS is p less its ADTS header (7 bytes, 9 with a CRC), its
// timestamps, duration and flags kept.
func withoutADTS(p *ffgo.Packet) (*ffgo.Packet, error) {
	d := p.Data()
	if len(d) < 7 || d[0] != 0xFF || d[1]&0xF0 != 0xF0 {
		return nil, fmt.Errorf("%w: copied AAC that is not ADTS", video.ErrShape)
	}
	if d[6]&0x03 != 0 {
		return nil, fmt.Errorf("%w: an ADTS frame of several AAC frames", video.ErrShape)
	}
	n := 7
	if d[1]&0x01 == 0 {
		n = 9
	}
	if len(d) <= n {
		return nil, fmt.Errorf("%w: an empty ADTS frame", video.ErrShape)
	}
	q, err := ffgo.NewPacketFromData(d[n:])
	if err != nil {
		return nil, err
	}
	src, dst := p.Raw(), q.Raw()
	avcodec.SetPacketPTS(dst, p.PTS())
	avcodec.SetPacketDTS(dst, p.DTS())
	avcodec.SetPacketDuration(dst, avcodec.GetPacketDuration(src))
	avcodec.SetPacketFlags(dst, avcodec.GetPacketFlags(src))
	avcodec.SetPacketStreamIndex(dst, int32(p.StreamIndex()))
	return q, nil
}

// adtsConfig is the AudioSpecificConfig an ADTS header describes, as
// FFmpeg's aac_adtstoasc builds it (ISO/IEC 14496-3 1.6.2.1): its object
// type, sampling frequency index and channel configuration. The cutter
// strips each packet's header (withoutADTS).
func adtsConfig(p []byte) ([]byte, error) {
	if len(p) < 7 || p[0] != 0xFF || p[1]&0xF0 != 0xF0 {
		return nil, fmt.Errorf("%w: copied AAC that is not ADTS", video.ErrShape)
	}
	object := p[2]>>6 + 1
	freq := (p[2] >> 2) & 0x0F
	channels := (p[2]&0x01)<<2 | p[3]>>6
	if freq > 12 || channels == 0 {
		// Channels 0 is a layout in a program config element, which no
		// capture carries.
		return nil, fmt.Errorf("%w: an ADTS header the pool does not copy", video.ErrShape)
	}
	v := uint16(object)<<11 | uint16(freq)<<7 | uint16(channels)<<3
	return []byte{byte(v >> 8), byte(v)}, nil
}

// avcCodecs is the RFC 6381 name of the H.264 stream an init describes:
// avc1.PPCCLL from its avcC box, or avc1 alone.
func avcCodecs(init []byte) string {
	i := bytes.Index(init, []byte("avcC"))
	if i < 0 || i+8 > len(init) {
		return "avc1"
	}
	return fmt.Sprintf("avc1.%02x%02x%02x", init[i+5], init[i+6], init[i+7])
}

func (c *dashCutter) frame() time.Duration {
	r := c.job.Video.FrameRate
	return time.Duration(int64(time.Second) * int64(r.Den) / int64(r.Num))
}

// boundary is the earliest the open segment can end: n segment durations.
func (c *dashCutter) boundary() time.Duration {
	return time.Duration(c.n) * c.job.Segments.Duration
}

func (c *dashCutter) video(p *ffgo.Packet) error {
	pts := toTime(p.PTS(), c.v.tb)
	key := avcodec.GetPacketFlags(p.Raw())&avcodec.PacketFlagKey != 0
	if key && c.open && pts >= c.boundary() {
		if err := c.cut(pts); err != nil {
			return err
		}
	}
	if !c.open {
		if !key {
			return errors.New("the video does not start on a keyframe")
		}
		c.open = true
	}
	c.last = pts
	return c.v.write(p, packetEnd(p, c.v.tb, c.frame()))
}

// audio writes an audio packet into the open segment, or holds it while
// it may belong to the next: the video's keyframe decides where that starts.
func (c *dashCutter) audio(p *ffgo.Packet) error {
	if c.adts && c.asc == nil {
		var err error
		if c.asc, err = adtsConfig(p.Data()); err != nil {
			return err
		}
	}
	q, err := c.queue(p)
	if err != nil {
		return err
	}
	c.aq = append(c.aq, q)
	if c.a == nil && c.v != nil && c.asc != nil {
		if err := c.openAudioRep(c.asc); err != nil {
			return err
		}
	}
	return c.writeAudio(c.boundary())
}

func (c *dashCutter) queue(p *ffgo.Packet) (queued, error) {
	clone := p.Clone
	if c.adts {
		clone = func() (*ffgo.Packet, error) { return withoutADTS(p) }
	}
	cl, err := clone()
	if err != nil {
		return queued{}, err
	}
	return queued{p: cl, pts: toTime(cl.PTS(), c.atb), end: packetEnd(cl, c.atb, 0)}, nil
}

// writeAudio writes the held audio before until, in order; until the video
// encoder has opened the representations, it holds it all.
func (c *dashCutter) writeAudio(until time.Duration) error {
	if c.a == nil || c.a.ms == nil {
		return nil
	}
	for len(c.aq) > 0 && c.aq[0].pts < until {
		q := c.aq[0]
		c.aq = c.aq[1:]
		err := c.a.write(q.p, q.end)
		_ = q.p.Free()
		if err != nil {
			return err
		}
	}
	if len(c.aq) > maxQueued {
		return errors.New("audio ran ahead of the video by more than the queue holds")
	}
	return nil
}

// cut ends segment n at the video keyframe at, and sends it.
func (c *dashCutter) cut(at time.Duration) error {
	if c.a == nil {
		return errors.New("no audio by the end of the first segment")
	}
	if err := c.writeAudio(at); err != nil {
		return err
	}
	vf, err := c.v.cut()
	if err != nil {
		return err
	}
	af, err := c.a.cut()
	if err != nil {
		return err
	}
	return c.emit(vf, af)
}

func (c *dashCutter) emit(vf, af remux.Fragment) error {
	if !c.sentInit {
		if err := c.sink.File(remux.InitFile(0), c.v.init); err != nil {
			return err
		}
		if err := c.sink.File(remux.InitFile(1), c.a.init); err != nil {
			return err
		}
		c.sentInit = true
	}
	if err := c.sink.File(remux.ChunkFile(0, c.n), vf.Data); err != nil {
		return err
	}
	if err := c.sink.File(remux.ChunkFile(1, c.n), af.Data); err != nil {
		return err
	}
	c.manifest.Video.Timeline = append(c.manifest.Video.Timeline, remux.TimelineEntry{T: vf.T, D: vf.D})
	c.manifest.Audio.Timeline = append(c.manifest.Audio.Timeline, remux.TimelineEntry{T: af.T, D: af.D})
	if w := c.job.Segments.ListSize; c.live && w > 0 && len(c.manifest.Video.Timeline) > w {
		drop := len(c.manifest.Video.Timeline) - w
		c.manifest.Video.Timeline = c.manifest.Video.Timeline[drop:]
		c.manifest.Audio.Timeline = c.manifest.Audio.Timeline[drop:]
		c.manifest.StartNumber += drop
	}
	c.manifest.Now = c.now()
	c.n++
	return c.sink.Manifest(c.manifest.Render())
}

// finish sends the last segment and the static manifest.
func (c *dashCutter) finish() error {
	if c.v == nil {
		return errors.New("no video was encoded")
	}
	if !c.open {
		return errors.New("no segment was written")
	}
	if c.a == nil {
		return errors.New("no audio was written")
	}
	if err := c.writeAudio(time.Duration(1<<62 - 1)); err != nil {
		return err
	}
	vf, err := c.v.cut()
	if err != nil {
		return err
	}
	af, err := c.a.cut()
	if err != nil {
		return err
	}
	if err := c.emit(vf, af); err != nil {
		return err
	}
	c.manifest.Final = true
	if c.manifest.Duration <= 0 {
		c.manifest.Duration = c.last
	}
	return c.sink.Manifest(c.manifest.Render())
}

func (c *dashCutter) close() {
	for _, q := range c.aq {
		_ = q.p.Free()
	}
	c.aq = nil
	for _, r := range []*dashRep{c.v, c.a} {
		if r != nil {
			_ = r.m.Close()
		}
	}
}
