package pipeline

import (
	"fmt"
	"time"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/obinnaokechukwu/ffgo/avcodec"
	"github.com/obinnaokechukwu/ffgo/avutil"

	"github.com/mediactl/clusterplex/pkg/video"
)

// audioPath is the mapped audio: copied, or decoded, resampled to Plex's
// layout and rate (its aresample) and encoded to AAC at Plex's bit rate.
type audioPath struct {
	job    video.Job
	in     *input
	c      segmenter
	sd     *ffgo.StreamDecoder
	enc    *ffgo.AudioEncoder
	res    *ffgo.Resampler
	rate   int
	layout string
	shift  int64 // -start_at_zero's shift in the decoder's time base
}

func newAudioPath(job video.Job, in *input, c segmenter) (*audioPath, error) {
	a, src := job.Audio, in.audio
	ap := &audioPath{job: job, in: in, c: c}
	if a.Copy {
		c.openAudioCopy(src)
		return ap, nil
	}
	ap.rate = a.SampleRate
	if ap.rate == 0 {
		ap.rate = src.SampleRate
	}
	switch a.Channels {
	case 1:
		ap.layout = "mono"
	case 2:
		ap.layout = "stereo"
	default:
		ap.layout = src.ChannelLayout
	}
	sd, err := in.d.NewStreamDecoder(src.Index, nil)
	if err != nil {
		return nil, fmt.Errorf("audio decoder: %w", err)
	}
	ap.sd = sd
	ap.shift = int64(in.shift.Seconds() * float64(sd.TimeBase().Den) / float64(sd.TimeBase().Num))
	if ap.enc, err = ffgo.NewAudioEncoder(ffgo.AudioEncoderConfig2{
		EncoderName: "aac", SampleRate: ap.rate, Layout: ap.layout, BitRate: a.BitRate, InputTimeBase: sd.TimeBase(),
		GlobalHeader: c.globalHeader(),
	}); err != nil {
		_ = sd.Close()
		return nil, fmt.Errorf("aac: %w", err)
	}
	c.openAudio(ap.enc)
	return ap, nil
}

func (a *audioPath) close() {
	if a.res != nil {
		_ = a.res.Close()
	}
	if a.enc != nil {
		_ = a.enc.Close()
	}
	if a.sd != nil {
		_ = a.sd.Close()
	}
}

// send takes one audio packet; one before -ss is dropped, as Plex's
// accurate seek drops it.
func (a *audioPath) send(p *ffgo.Packet) error {
	src := a.in.audio
	if ts := p.PTS(); a.job.Start > 0 && ts != avutil.AV_NOPTS_VALUE && a.in.at(ts, src.TimeBase) < a.job.Start {
		return nil
	}
	if a.sd == nil {
		c, err := p.Clone()
		if err != nil {
			return err
		}
		defer func() { _ = c.Free() }()
		if a.in.shift > 0 {
			off := int64(a.in.shift.Seconds() * float64(src.TimeBase.Den) / float64(src.TimeBase.Num))
			for _, set := range []struct {
				get func() int64
				put func(int64)
			}{
				{c.PTS, func(v int64) { avcodec.SetPacketPTS(c.Raw(), v) }},
				{c.DTS, func(v int64) { avcodec.SetPacketDTS(c.Raw(), v) }},
			} {
				if v := set.get(); v != avutil.AV_NOPTS_VALUE {
					set.put(v - off)
				}
			}
		}
		return a.c.audio(c)
	}
	return decode(a.sd, p, a.frame)
}

func (a *audioPath) frame(f ffgo.Frame) error {
	var err error
	if a.res == nil {
		if a.res, err = ffgo.NewResampler(
			ffgo.AudioFormat{SampleRate: a.in.audio.SampleRate, Layout: f.ChannelLayout(), SampleFormat: ffgo.SampleFormat(f.Format())},
			ffgo.AudioFormat{SampleRate: a.rate, Layout: a.layout, SampleFormat: ffgo.SampleFormatFLTP}); err != nil {
			return fmt.Errorf("resampler: %w", err)
		}
	}
	r, err := a.res.Resample(f)
	if err != nil {
		return err
	}
	if r.IsNil() {
		return nil
	}
	defer func() { _ = r.Free() }()
	r.SetPTS(f.PTS() - a.shift)
	return a.enc.Encode(r, a.c.audio)
}

func (a *audioPath) flush() error {
	if a.sd == nil {
		return nil
	}
	if err := decode(a.sd, nil, a.frame); err != nil {
		return err
	}
	if a.res != nil {
		tail, err := a.res.Flush()
		if err == nil && !tail.IsNil() {
			err = a.enc.Encode(tail, a.c.audio)
			_ = tail.Free()
		}
		if err != nil {
			return err
		}
	}
	return a.enc.Flush(a.c.audio)
}

// packetEnd is a packet's end time: its pts plus its duration, or frame
// when it carries none.
func packetEnd(p *ffgo.Packet, tb ffgo.Rational, frame time.Duration) time.Duration {
	if d := avcodec.GetPacketDuration(p.Raw()); d > 0 {
		return toTime(p.PTS()+d, tb)
	}
	return toTime(p.PTS(), tb) + frame
}
