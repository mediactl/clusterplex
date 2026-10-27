// Package video is a Plex Transcoder job that converts video, as the GPU
// pool runs it (docs/superpowers/specs/2026-10-26-gpu-video-transcode-pool-design.md):
// the parser that reads Plex's argv against an allow-list built from
// captured jobs, and the rules Plex's segments follow. It links no ffgo: the
// manager parses and routes, and only cmd/video-worker converts.
package video

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

// ErrShape is a Plex job the pool does not run: an option, filter, codec
// or muxer no captured conversion carries. Such a job is refused, never
// tried anyway and never run on Plex's node.
var ErrShape = errors.New("not a video conversion the pool runs")

// Reason is why a conversion was refused: the bounded value of the
// refusal metric's reason label, and the first word of the shim's stderr.
type Reason string

const (
	ReasonNoPool      Reason = "no-pool"      // no ready pool pod, or no pool configured
	ReasonFull        Reason = "full"         // every ready pod's slots are taken
	ReasonShape       Reason = "shape"        // the parser refused the argv
	ReasonWorkerError Reason = "worker-error" // a dial, the worker or a write failed
	ReasonCancelled   Reason = "cancelled"    // Plex ended the job
)

// OutputKind is what a conversion writes.
type OutputKind string

const (
	OutputHLS  OutputKind = "hls"  // MPEG-TS segments and a segment list (-f ssegment)
	OutputDASH OutputKind = "dash" // the remux pool's fragmented MP4 segments and MPD
	OutputFile OutputKind = "file" // one file: a download, an Optimize, a DVR conversion
)

// Rational is a frame rate.
type Rational struct{ Num, Den int }

// Input is one -i.
type Input struct {
	// Path is a file under /media, which the worker reads from its own
	// read-only mount of the library.
	Path string
	// Streamed is every other input (Live TV's session on Plex, a subtitle
	// Plex downloaded), which the manager reads and streams to the worker.
	Streamed bool
	// Format is a streamed input's demuxer; empty lets FFmpeg probe.
	Format string
	// Name is a streamed file's base name, e.g. "sub.srt".
	Name string
	// Source is the manager's alone, never on the wire: the URL or local
	// path it streams.
	Source string
}

// VideoOut is the converted video stream.
type VideoOut struct {
	Stream        int // the input video stream
	Width, Height int
	PixFmt        string // nv12 or yuv420p: 8-bit 4:2:0 either way
	CRF           int
	MaxRate       int64 // bits per second
	BufSize       int64 // bits
	FrameRate     Rational
	KeyframeEvery time.Duration // -force_key_frames expr:gte(t,n_forced*N)
	Deinterlace   bool
	ToneMap       bool // HDR to SDR
}

// AudioOut is the audio stream: converted to AAC, or copied.
type AudioOut struct {
	Stream     int
	Copy       bool
	Channels   int
	SampleRate int
	BitRate    int64
	Language   string
}

// BurnIn is a subtitle drawn into the picture.
type BurnIn struct {
	Input  int // index into Job.Inputs
	Stream int // the subtitle stream's index within that input
	Image  bool
	// Style is a text subtitle's ASS style, as libass's force_style takes
	// it: Plex's inlineass overrides plus its font size, outline and
	// shadow (finding B).
	Style string
}

// Segmenting is how an HLS or DASH output is cut.
type Segmenting struct {
	Duration    time.Duration // -segment_time
	StartNumber int           // -segment_start_number
	Pattern     string        // the output's name pattern, e.g. media-%05d.ts
	ListSize    int           // -segment_list_size
	TimeDelta   time.Duration // -segment_time_delta
}

// FileOut is a whole-file output's container.
type FileOut struct {
	Format       string
	MuxerOptions map[string]string
}

// Job is one video conversion, as Plex asked for it.
type Job struct {
	Output   OutputKind
	Session  string // from -progressurl's /session/<s>/
	Inputs   []Input
	Start    time.Duration // -ss
	Video    VideoOut
	Audio    *AudioOut // nil: no audio output
	Burn     *BurnIn   // nil: no burn-in
	Segments Segmenting
	File     FileOut

	// The manager's alone; never on the wire (wire.go).
	Token          string // the job's X_PLEX_TOKEN
	ProgressURL    string
	SegmentListURL string
	ManifestURL    string // DASH's -manifest_name
	OutputPath     string // a whole file's destination
}

// RingKey breaks ties between equally free workers: a library file, else
// the session.
func (j Job) RingKey() string {
	if len(j.Inputs) > 0 && j.Inputs[0].Path != "" {
		return j.Inputs[0].Path
	}
	return j.Session
}

// sessionOf is Plex's transcode session id, from a progress URL's
// /video/:/transcode/session/<id>/<uuid>/progress.
func sessionOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if p == "session" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}
