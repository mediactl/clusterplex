package video

import (
	"maps"
	"time"

	"github.com/mediactl/clusterplex/proto/videopb"
)

// AcceptedHeader is the response header a worker sends, before any output,
// when it takes a job: the relay then streams the job's inputs. A worker
// that refuses answers with a status instead (RESOURCE_EXHAUSTED when its
// slots are taken, INVALID_ARGUMENT for a job it cannot run).
const AcceptedHeader = "video-accepted"

// Proto is the job as the worker is sent it: without the token, without
// any URL, and without a streamed input's source, since the pool parses
// untrusted media and holds no Plex secret (spec §5).
func (j Job) Proto() *videopb.Job {
	p := &videopb.Job{
		Output:  string(j.Output),
		Session: j.Session,
		StartUs: j.Start.Microseconds(),
		Video: &videopb.VideoOut{
			Stream: int32(j.Video.Stream), Width: int32(j.Video.Width), Height: int32(j.Video.Height),
			PixFmt: j.Video.PixFmt, Crf: int32(j.Video.CRF), MaxRate: j.Video.MaxRate, BufSize: j.Video.BufSize,
			RateNum: int32(j.Video.FrameRate.Num), RateDen: int32(j.Video.FrameRate.Den),
			KeyframeEveryUs: j.Video.KeyframeEvery.Microseconds(),
			Deinterlace:     j.Video.Deinterlace, ToneMap: j.Video.ToneMap,
		},
		Segments: &videopb.Segmenting{
			DurationUs: j.Segments.Duration.Microseconds(), StartNumber: int32(j.Segments.StartNumber),
			Pattern: j.Segments.Pattern, ListSize: int32(j.Segments.ListSize), TimeDeltaUs: j.Segments.TimeDelta.Microseconds(),
		},
		File: &videopb.FileOut{Format: j.File.Format, MuxerOptions: maps.Clone(j.File.MuxerOptions)},
	}
	for _, in := range j.Inputs {
		p.Inputs = append(p.Inputs, &videopb.Input{Path: in.Path, Streamed: in.Streamed, Format: in.Format, Name: in.Name})
	}
	if a := j.Audio; a != nil {
		p.Audio = &videopb.AudioOut{
			Stream: int32(a.Stream), Copy: a.Copy, Channels: int32(a.Channels), SampleRate: int32(a.SampleRate),
			BitRate: a.BitRate, Language: a.Language,
		}
	}
	if b := j.Burn; b != nil {
		p.Burn = &videopb.BurnIn{Input: int32(b.Input), Stream: int32(b.Stream), Image: b.Image}
	}
	return p
}

// JobFromProto is the worker's side of Proto.
func JobFromProto(p *videopb.Job) Job {
	v, s, f := p.GetVideo(), p.GetSegments(), p.GetFile()
	j := Job{
		Output:  OutputKind(p.GetOutput()),
		Session: p.GetSession(),
		Start:   time.Duration(p.GetStartUs()) * time.Microsecond,
		Video: VideoOut{
			Stream: int(v.GetStream()), Width: int(v.GetWidth()), Height: int(v.GetHeight()),
			PixFmt: v.GetPixFmt(), CRF: int(v.GetCrf()), MaxRate: v.GetMaxRate(), BufSize: v.GetBufSize(),
			FrameRate:     Rational{Num: int(v.GetRateNum()), Den: int(v.GetRateDen())},
			KeyframeEvery: time.Duration(v.GetKeyframeEveryUs()) * time.Microsecond,
			Deinterlace:   v.GetDeinterlace(), ToneMap: v.GetToneMap(),
		},
		Segments: Segmenting{
			Duration: time.Duration(s.GetDurationUs()) * time.Microsecond, StartNumber: int(s.GetStartNumber()),
			Pattern: s.GetPattern(), ListSize: int(s.GetListSize()), TimeDelta: time.Duration(s.GetTimeDeltaUs()) * time.Microsecond,
		},
		File: FileOut{Format: f.GetFormat()},
	}
	if len(f.GetMuxerOptions()) > 0 {
		j.File.MuxerOptions = maps.Clone(f.GetMuxerOptions())
	}
	for _, in := range p.GetInputs() {
		j.Inputs = append(j.Inputs, Input{Path: in.GetPath(), Streamed: in.GetStreamed(), Format: in.GetFormat(), Name: in.GetName()})
	}
	if a := p.GetAudio(); a != nil {
		j.Audio = &AudioOut{
			Stream: int(a.GetStream()), Copy: a.GetCopy(), Channels: int(a.GetChannels()), SampleRate: int(a.GetSampleRate()),
			BitRate: a.GetBitRate(), Language: a.GetLanguage(),
		}
	}
	if b := p.GetBurn(); b != nil {
		j.Burn = &BurnIn{Input: int(b.GetInput()), Stream: int(b.GetStream()), Image: b.GetImage()}
	}
	return j
}
