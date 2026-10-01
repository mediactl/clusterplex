// Package plexseed writes what Plex's scanner would -- a file's streams --
// and TheIntroDB's skip segments into Plex's PostgreSQL library, from a
// clustarr MediaFile's probe and status.markers. Plex's API can write
// neither (ADR 0006); the scanner is bypassed, not replaced: seeded rows
// are marked, a file Plex analysed is never touched, and Plex's own
// analysis replaces ours when it runs.
package plexseed

// Input is what the seeder reads of a clustarr MediaFile. The field names
// mirror clustarr's JSON (MediaFile spec and status); nothing of
// clustarr's code is imported.
type Input struct {
	// Path is spec.path, clustarr's own path; the caller maps it to Plex's.
	Path      string
	SizeBytes int64
	ProbeHash string
	Probe     *Probe
	Markers   *Markers
}

// Probe is status.mediaInfo.
type Probe struct {
	Container        string     `json:"container,omitempty"`
	VideoCodec       string     `json:"videoCodec,omitempty"`
	VideoProfile     string     `json:"videoProfile,omitempty"`
	PixelFormat      string     `json:"pixelFormat,omitempty"`
	VideoBitDepth    int32      `json:"videoBitDepth,omitempty"`
	Width            int32      `json:"width,omitempty"`
	Height           int32      `json:"height,omitempty"`
	FpsMilli         int32      `json:"fpsMilli,omitempty"`
	VideoBitrateKbps int32      `json:"videoBitrateKbps,omitempty"`
	RuntimeMillis    int64      `json:"runtimeMillis,omitempty"`
	Audio            []Audio    `json:"audio,omitempty"`
	Subtitles        []Subtitle `json:"subtitles,omitempty"`
}

// Audio is one audio track of the probe.
type Audio struct {
	Index         int32  `json:"index,omitempty"`
	Codec         string `json:"codec,omitempty"`
	Profile       string `json:"profile,omitempty"`
	Language      string `json:"language,omitempty"`
	Title         string `json:"title,omitempty"`
	Channels      int32  `json:"channels,omitempty"`
	ChannelLayout string `json:"channelLayout,omitempty"`
	BitrateKbps   int32  `json:"bitrateKbps,omitempty"`
	Default       bool   `json:"default,omitempty"`
}

// Subtitle is one subtitle track of the probe.
type Subtitle struct {
	Index           int32  `json:"index,omitempty"`
	Codec           string `json:"codec,omitempty"`
	Language        string `json:"language,omitempty"`
	Title           string `json:"title,omitempty"`
	Forced          bool   `json:"forced,omitempty"`
	HearingImpaired bool   `json:"hearingImpaired,omitempty"`
}

// Markers is status.markers: TheIntroDB's Result, clustarr's own Analysis,
// and Segments, the merge of the two.
type Markers struct {
	Result   string    `json:"result"`
	Segments []Segment `json:"segments,omitempty"`
	Analysis *Analysis `json:"analysis,omitempty"`
}

// Analysis is status.markers.analysis: clustarr's own detection's result.
type Analysis struct {
	Result string `json:"result"`
}

// Segment is one skip segment: intro, recap, credits or preview.
type Segment struct {
	Kind    string `json:"kind"`
	StartMs int64  `json:"startMs"`
	EndMs   int64  `json:"endMs"`
	// Source is where clustarr got it: theintrodb, chapters or analysis;
	// empty is theintrodb, which wrote segments before sources existed.
	Source     string `json:"source,omitempty"`
	Confidence int32  `json:"confidence,omitempty"`
}
