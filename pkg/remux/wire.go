package remux

import (
	"time"

	"github.com/mediactl/clusterplex/proto/remuxpb"
)

// Proto is the job as a worker receives it: no token, no Plex URL.
func (j Job) Proto() *remuxpb.Job {
	return &remuxpb.Job{
		Input: j.Input, StartMs: j.Start.Milliseconds(), SkipToSegment: int32(j.SkipToSegment),
		SegmentMs: j.SegmentDuration.Milliseconds(), VideoStream: int32(j.VideoStream),
		AudioStream: int32(j.AudioStream), AudioCopy: j.AudioCopy, AudioChannels: int32(j.AudioChannels),
		AudioSampleRate: int32(j.AudioSampleRate), AudioBitRate: j.AudioBitRate, AudioLanguage: j.AudioLanguage,
	}
}

func JobFromProto(p *remuxpb.Job) Job {
	return Job{
		Input: p.GetInput(), Start: time.Duration(p.GetStartMs()) * time.Millisecond,
		SkipToSegment: int(p.GetSkipToSegment()), SegmentDuration: time.Duration(p.GetSegmentMs()) * time.Millisecond,
		VideoStream: int(p.GetVideoStream()), AudioStream: int(p.GetAudioStream()), AudioCopy: p.GetAudioCopy(),
		AudioChannels: int(p.GetAudioChannels()), AudioSampleRate: int(p.GetAudioSampleRate()),
		AudioBitRate: p.GetAudioBitRate(), AudioLanguage: p.GetAudioLanguage(),
	}
}
