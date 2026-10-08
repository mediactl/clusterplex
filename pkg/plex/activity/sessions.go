// Package activity is what Tautulli reads from Plex, read by the manager
// instead: every playback a pod's Plex is serving, how it is being served
// (direct play, direct stream or transcode; LAN or WAN; on what hardware),
// each play from its first sighting to its end, and the size of the library.
// It publishes the aggregates as Prometheus series and each finished play as
// an OpenTelemetry span, since titles are fine in a trace and unbounded as a
// label.
package activity

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

// PMS is the part of Plex's API this package reads: a GET whose JSON answer
// is decoded into out (pkg/plex/api.Client).
type PMS interface {
	Get(ctx context.Context, path string, q url.Values, out any) error
}

// Decisions, as Tautulli names them.
const (
	DirectPlay   = "direct_play"
	DirectStream = "direct_stream"
	Transcode    = "transcode"
)

// Stream is one playback in /status/sessions.
type Stream struct {
	// Key identifies the playback across reads: its session and the item, so
	// a player that moves on to the next episode under the same session
	// ends one play and starts another.
	Key string
	// RatingKey is the item's id in Plex.
	RatingKey string
	// MediaType is Plex's type (movie, episode, track, clip, photo), or
	// "live" for Live TV.
	MediaType        string
	Title            string
	GrandparentTitle string
	ParentIndex      int
	Index            int
	Year             int

	User     string
	Player   string // the player's product, e.g. "Plex Web"
	Platform string // e.g. "Chrome", "Android"
	Device   string
	// State is the player's: playing, paused or buffering.
	State string
	// Address is the client's public address when Plex knows it, else its
	// local one.
	Address string
	// Location is lan or wan.
	Location string
	Relayed  bool
	Secure   bool
	// BandwidthKbps is what Plex budgets for the stream, in kilobits a
	// second.
	BandwidthKbps int64

	// ViewOffset and Duration are in milliseconds.
	ViewOffset int64
	Duration   int64

	// Resolution is the source's video resolution (4k, 1080, 720, 480, sd);
	// empty for audio.
	Resolution string
	VideoCodec string
	AudioCodec string

	// Transcoding is whether Plex runs a transcode session for it, and the
	// fields after it are that session's.
	Transcoding      bool
	VideoDecision    string
	AudioDecision    string
	SubtitleDecision string
	HWDecode         string // the decoder's API (vaapi, nvdec, ...), or ""
	HWEncode         string
	Throttled        bool
	Complete         bool
	// Speed is the transcode's speed relative to real time.
	Speed float64
}

// Decision is how the stream is served, as Tautulli decides it: a transcode
// when the video or the audio is transcoded, a direct stream when either is
// copied into a new container, else direct play.
func (s Stream) Decision() string {
	switch {
	case !s.Transcoding:
		return DirectPlay
	case s.VideoDecision == "transcode" || s.AudioDecision == "transcode":
		return Transcode
	case s.VideoDecision == "copy" || s.AudioDecision == "copy":
		return DirectStream
	default:
		return DirectPlay
	}
}

// Lagging reports a transcode that is not keeping up: slower than real time,
// and neither throttled (ahead of the player) nor finished.
func (s Stream) Lagging() bool {
	return s.Transcoding && !s.Throttled && !s.Complete && s.Speed > 0 && s.Speed < 1
}

// Progress is how far through the item the player is, 0 to 1; 0 when the
// item has no duration (Live TV).
func (s Stream) Progress() float64 {
	if s.Duration <= 0 {
		return 0
	}
	return min(float64(s.ViewOffset)/float64(s.Duration), 1)
}

// FullTitle names the item the way Tautulli lists it: "Show - S01E02 -
// Episode", "Artist - Track", or the title alone.
func (s Stream) FullTitle() string {
	switch {
	case s.MediaType == "episode" && s.GrandparentTitle != "":
		return s.GrandparentTitle + " - S" + pad2(s.ParentIndex) + "E" + pad2(s.Index) + " - " + s.Title
	case s.GrandparentTitle != "":
		return s.GrandparentTitle + " - " + s.Title
	default:
		return s.Title
	}
}

func pad2(n int) string {
	if n < 10 && n >= 0 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// sessionsPath lists what Plex is serving right now.
const sessionsPath = "/status/sessions"

// ReadSessions reads what Plex is serving.
func ReadSessions(ctx context.Context, pms PMS) ([]Stream, error) {
	var out sessionsDoc
	if err := pms.Get(ctx, sessionsPath, nil, &out); err != nil {
		return nil, err
	}
	streams := make([]Stream, 0, len(out.MediaContainer.Metadata))
	for _, m := range out.MediaContainer.Metadata {
		streams = append(streams, m.stream())
	}
	return streams, nil
}

// sessionsDoc is /status/sessions in JSON. Plex lists every playing item
// under Metadata, whatever its kind.
type sessionsDoc struct {
	MediaContainer struct {
		Metadata []sessionItem `json:"Metadata"`
	} `json:"MediaContainer"`
}

type sessionItem struct {
	RatingKey        flexString `json:"ratingKey"`
	SessionKey       flexString `json:"sessionKey"`
	Type             string     `json:"type"`
	Title            string     `json:"title"`
	GrandparentTitle string     `json:"grandparentTitle"`
	ParentIndex      flexInt    `json:"parentIndex"`
	Index            flexInt    `json:"index"`
	Year             flexInt    `json:"year"`
	Live             flexBool   `json:"live"`
	ViewOffset       flexInt    `json:"viewOffset"`
	Duration         flexInt    `json:"duration"`
	Media            []struct {
		Selected        flexBool `json:"selected"`
		VideoResolution string   `json:"videoResolution"`
		VideoCodec      string   `json:"videoCodec"`
		AudioCodec      string   `json:"audioCodec"`
	} `json:"Media"`
	User struct {
		Title string `json:"title"`
	} `json:"User"`
	Player struct {
		Product             string   `json:"product"`
		Platform            string   `json:"platform"`
		Device              string   `json:"device"`
		State               string   `json:"state"`
		Address             string   `json:"address"`
		RemotePublicAddress string   `json:"remotePublicAddress"`
		Local               flexBool `json:"local"`
		Relayed             flexBool `json:"relayed"`
		Secure              flexBool `json:"secure"`
	} `json:"Player"`
	Session struct {
		ID        string  `json:"id"`
		Bandwidth flexInt `json:"bandwidth"`
		Location  string  `json:"location"`
	} `json:"Session"`
	TranscodeSession *struct {
		VideoDecision       string    `json:"videoDecision"`
		AudioDecision       string    `json:"audioDecision"`
		SubtitleDecision    string    `json:"subtitleDecision"`
		Throttled           flexBool  `json:"throttled"`
		Complete            flexBool  `json:"complete"`
		Speed               flexFloat `json:"speed"`
		TranscodeHwDecoding string    `json:"transcodeHwDecoding"`
		TranscodeHwEncoding string    `json:"transcodeHwEncoding"`
	} `json:"TranscodeSession"`
}

func (m sessionItem) stream() Stream {
	s := Stream{
		RatingKey:        string(m.RatingKey),
		MediaType:        m.Type,
		Title:            m.Title,
		GrandparentTitle: m.GrandparentTitle,
		ParentIndex:      int(m.ParentIndex),
		Index:            int(m.Index),
		Year:             int(m.Year),
		User:             m.User.Title,
		Player:           m.Player.Product,
		Platform:         m.Player.Platform,
		Device:           m.Player.Device,
		State:            m.Player.State,
		Address:          m.Player.RemotePublicAddress,
		Location:         m.Session.Location,
		Relayed:          bool(m.Player.Relayed),
		Secure:           bool(m.Player.Secure),
		BandwidthKbps:    int64(m.Session.Bandwidth),
		ViewOffset:       int64(m.ViewOffset),
		Duration:         int64(m.Duration),
	}
	if bool(m.Live) {
		s.MediaType = "live"
	}
	if s.Address == "" {
		s.Address = m.Player.Address
	}
	if s.Location == "" {
		// Older Plex names no location; Tautulli then goes by the player.
		s.Location = "wan"
		if bool(m.Player.Local) {
			s.Location = "lan"
		}
	}
	// Session.id is unique per playback; sessionKey is a small number Plex
	// reuses, so it is the fallback only.
	id := m.Session.ID
	if id == "" {
		id = string(m.SessionKey)
	}
	s.Key = id + "/" + s.RatingKey
	if len(m.Media) > 0 {
		media := m.Media[0]
		for _, md := range m.Media {
			if bool(md.Selected) {
				media = md
				break
			}
		}
		s.Resolution = strings.ToLower(media.VideoResolution)
		s.VideoCodec = media.VideoCodec
		s.AudioCodec = media.AudioCodec
	}
	if ts := m.TranscodeSession; ts != nil {
		s.Transcoding = true
		s.VideoDecision = ts.VideoDecision
		s.AudioDecision = ts.AudioDecision
		s.SubtitleDecision = ts.SubtitleDecision
		s.Throttled = bool(ts.Throttled)
		s.Complete = bool(ts.Complete)
		s.Speed = float64(ts.Speed)
		s.HWDecode = ts.TranscodeHwDecoding
		s.HWEncode = ts.TranscodeHwEncoding
	}
	return s
}

// Plex's JSON writes some attributes as strings, numbers or booleans
// depending on the endpoint and version (its XML has only strings), so these
// take any of them.
type (
	flexString string
	flexInt    int64
	flexFloat  float64
	flexBool   bool
)

func unquote(b []byte) []byte {
	b = bytes.TrimSpace(b)
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		var s string
		if json.Unmarshal(b, &s) == nil {
			return []byte(s)
		}
	}
	return b
}

func (f *flexString) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	*f = flexString(unquote(b))
	return nil
}

func (f *flexInt) UnmarshalJSON(b []byte) error {
	v := string(unquote(b))
	if v == "" || v == "null" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		// A fraction where Plex usually writes a whole number.
		x, ferr := strconv.ParseFloat(v, 64)
		if ferr != nil {
			return err
		}
		n = int64(x)
	}
	*f = flexInt(n)
	return nil
}

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	v := string(unquote(b))
	if v == "" || v == "null" {
		return nil
	}
	x, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return err
	}
	*f = flexFloat(x)
	return nil
}

func (f *flexBool) UnmarshalJSON(b []byte) error {
	switch strings.ToLower(string(unquote(b))) {
	case "true", "1":
		*f = true
	default:
		*f = false
	}
	return nil
}
