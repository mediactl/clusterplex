package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// universal is the prefix of Plex's universal transcoder; a master
// playlist names its variant relative to it.
const universal = "/video/:/transcode/universal/"

const (
	// maxPlaylist caps an HLS playlist: a variant lists a line per segment.
	maxPlaylist = 1 << 20
	// maxSegment caps a segment read: ten seconds of copied 4K video.
	maxSegment = 256 << 20
	// tsSync begins every MPEG-TS packet.
	tsSync = 0x47
)

// TranscodeClient is the Plex client a transcode is asked for. PMS decides
// by the client's profile -- its product and platform, plus what
// ProfileExtra (X-Plex-Client-Profile-Extra) adds to it -- so these headers,
// not the query, choose what is copied and what is converted.
type TranscodeClient struct {
	Identifier, Product, Version, Platform, PlatformVersion, Device, DeviceName string
	ProfileExtra                                                                string
}

func (tc TranscodeClient) header(session string) http.Header {
	h := http.Header{}
	for k, v := range map[string]string{
		"X-Plex-Client-Identifier":    tc.Identifier,
		"X-Plex-Product":              tc.Product,
		"X-Plex-Version":              tc.Version,
		"X-Plex-Platform":             tc.Platform,
		"X-Plex-Platform-Version":     tc.PlatformVersion,
		"X-Plex-Device":               tc.Device,
		"X-Plex-Device-Name":          tc.DeviceName,
		"X-Plex-Client-Profile-Extra": tc.ProfileExtra,
		"X-Plex-Session-Identifier":   session,
	} {
		if v != "" {
			h.Set(k, v)
		}
	}
	return h
}

// AudioTranscode is one HLS transcode of a library item's first media and
// part that asks PMS to copy the video and convert the audio: what a client
// gets when it can play the video but not the sound.
type AudioTranscode struct {
	Item    int64
	Session string
	Client  TranscodeClient
}

func (t AudioTranscode) query() url.Values {
	return url.Values{
		"path":              {"/library/metadata/" + strconv.FormatInt(t.Item, 10)},
		"mediaIndex":        {"0"},
		"partIndex":         {"0"},
		"protocol":          {"hls"},
		"fastSeek":          {"1"},
		"directPlay":        {"0"},
		"directStream":      {"1"},
		"directStreamAudio": {"0"},
		"audioBoost":        {"100"},
		"location":          {"lan"},
		"autoAdjustQuality": {"0"},
		"hasMDE":            {"1"},
		"session":           {t.Session},
	}
}

// StreamDecision is what PMS will do with one stream: "copy" or "transcode".
type StreamDecision struct {
	// StreamType is Plex's: 1 video, 2 audio, 3 subtitle.
	StreamType int    `json:"streamType"`
	Codec      string `json:"codec"`
	Decision   string `json:"decision"`
}

// TranscodeDecision is PMS's answer to a transcode it was asked about.
type TranscodeDecision struct {
	// Code is generalDecisionCode: 1001 is "conversion OK"; 2000 and up
	// refuse it, and Text says why.
	Code    int
	Text    string
	Streams []StreamDecision
}

// CopiesVideoConvertsAudio is whether PMS will run the transcode as asked:
// every video stream copied and at least one audio stream converted.
func (d TranscodeDecision) CopiesVideoConvertsAudio() bool {
	video, audio := false, false
	for _, s := range d.Streams {
		switch s.StreamType {
		case 1:
			if s.Decision != "copy" {
				return false
			}
			video = true
		case 2:
			audio = audio || s.Decision == "transcode"
		}
	}
	return video && audio
}

// DecideTranscode asks PMS what it would do, starting nothing.
func (c *Client) DecideTranscode(ctx context.Context, t AudioTranscode) (TranscodeDecision, error) {
	body, err := c.send(ctx, http.MethodGet, universal+"decision", t.query(), t.Client.header(t.Session), maxBody)
	if err != nil {
		return TranscodeDecision{}, err
	}
	var out struct {
		MediaContainer struct {
			GeneralDecisionCode   int    `json:"generalDecisionCode"`
			GeneralDecisionText   string `json:"generalDecisionText"`
			TranscodeDecisionText string `json:"transcodeDecisionText"`
			Metadata              []struct {
				Media []struct {
					Part []struct {
						Stream []StreamDecision `json:"Stream"`
					} `json:"Part"`
				} `json:"Media"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return TranscodeDecision{}, fmt.Errorf("GET %sdecision: decode: %w", universal, err)
	}
	mc := out.MediaContainer
	d := TranscodeDecision{Code: mc.GeneralDecisionCode, Text: mc.GeneralDecisionText}
	if mc.TranscodeDecisionText != "" {
		// The general text is "neither direct play nor conversion is
		// available"; the transcode text says what stood in the way.
		d.Text = mc.TranscodeDecisionText
	}
	for _, md := range mc.Metadata {
		for _, m := range md.Media {
			for _, p := range m.Part {
				d.Streams = append(d.Streams, p.Stream...)
			}
		}
	}
	return d, nil
}

// StartTranscode starts the transcode and returns its variant playlist's
// path, which lists the segments as the transcoder writes them.
func (c *Client) StartTranscode(ctx context.Context, t AudioTranscode) (string, error) {
	h := t.Client.header(t.Session)
	h.Set("Accept", "*/*")
	body, err := c.send(ctx, http.MethodGet, universal+"start.m3u8", t.query(), h, maxPlaylist)
	if err != nil {
		return "", err
	}
	rel, err := firstEntry(body)
	if err != nil {
		return "", fmt.Errorf("GET %sstart.m3u8: %w", universal, err)
	}
	return universal + rel, nil
}

// FirstSegment fetches the transcode's first segment and returns its size.
// PMS lists every segment of the file in the variant playlist from the
// start, so the list proves nothing; a segment it serves is one the
// transcoder wrote, which a transcoder short of a decoder never does.
func (c *Client) FirstSegment(ctx context.Context, t AudioTranscode, variant string) (int64, error) {
	h := t.Client.header(t.Session)
	h.Set("Accept", "*/*")
	body, err := c.send(ctx, http.MethodGet, variant, nil, h, maxPlaylist)
	if err != nil {
		return 0, err
	}
	rel, err := firstEntry(body)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", variant, err)
	}
	seg := path.Dir(variant) + "/" + rel
	resp, err := c.open(ctx, http.MethodGet, seg, nil, h)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var sync [1]byte
	if _, err := io.ReadFull(resp.Body, sync[:]); err != nil {
		return 0, fmt.Errorf("GET %s: %w", seg, err)
	}
	if sync[0] != tsSync {
		return 0, fmt.Errorf("GET %s: not an MPEG-TS segment (first byte %#x)", seg, sync[0])
	}
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxSegment))
	if err != nil {
		return 0, fmt.Errorf("GET %s: read: %w", seg, err)
	}
	return 1 + n, nil
}

// firstEntry is a playlist's first URI, relative to the playlist and
// without its query.
func firstEntry(playlist []byte) (string, error) {
	for _, line := range strings.Split(string(playlist), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rel, _, _ := strings.Cut(line, "?")
		if strings.Contains(rel, "..") || strings.HasPrefix(rel, "/") || strings.Contains(rel, "://") {
			return "", fmt.Errorf("unexpected playlist entry %q", rel)
		}
		return rel, nil
	}
	return "", errors.New("the playlist lists nothing")
}

// StopTranscode ends the session, its transcoder with it.
func (c *Client) StopTranscode(ctx context.Context, t AudioTranscode) error {
	h := t.Client.header(t.Session)
	h.Set("Accept", "*/*")
	_, err := c.send(ctx, http.MethodGet, universal+"stop", url.Values{"session": {t.Session}}, h, maxBody)
	return err
}

// ServerVersion is the PMS version, as /identity reports it.
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	var out struct {
		MediaContainer struct {
			Version string `json:"version"`
		} `json:"MediaContainer"`
	}
	if err := c.do(ctx, http.MethodGet, "/identity", nil, &out); err != nil {
		return "", err
	}
	if out.MediaContainer.Version == "" {
		return "", errors.New("plexapi: /identity names no version")
	}
	return out.MediaContainer.Version, nil
}
