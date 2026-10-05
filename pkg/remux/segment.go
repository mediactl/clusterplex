package remux

import "time"

// Fragment is one representation's part of a segment: a moof+mdat.
type Fragment struct {
	Data []byte
	T, D int64
}

// Segment is segment N of a stream. Start and End are source time, so a
// cached segment continues a chain only from a segment that ended where it
// starts.
type Segment struct {
	N            int
	Start, End   time.Duration
	Video, Audio Fragment
}

// StreamInfo is what the manifest says of the streams; the cache keeps it
// so a replay can publish a manifest without opening the file.
type StreamInfo struct {
	VideoCodec, AudioCodec string
	Width, Height          int
	FrameRate              string
	SampleRate, Channels   int
	Duration               time.Duration
}
