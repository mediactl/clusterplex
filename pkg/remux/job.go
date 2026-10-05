// Package remux runs Plex Web's DASH streams -- a copy of the video and the
// audio converted to stereo AAC -- on cluster-plex's remux pool instead of
// Plex's transcoder (docs/superpowers/specs/2026-10-01-browser-remux-transcoder-design.md).
// This package is what the manager links: it never imports ffgo.
package remux

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ErrNotRemux marks a job this package will not run, so Plex's transcoder does.
var ErrNotRemux = errors.New("not a browser remux")

// Plex's segment names, which PMS serves from the session directory.
const (
	InitName  = "init-stream$RepresentationID$.m4s"
	MediaName = "chunk-stream$RepresentationID$-$Number%05d$.m4s"
)

// InitFile and ChunkFile are the names of representation rep's files.
func InitFile(rep int) string     { return fmt.Sprintf("init-stream%d.m4s", rep) }
func ChunkFile(rep, n int) string { return fmt.Sprintf("chunk-stream%d-%05d.m4s", rep, n) }

// Job is a browser remux as Plex asked for it.
type Job struct {
	Input           string
	Start           time.Duration
	SkipToSegment   int
	SegmentDuration time.Duration
	VideoStream     int
	AudioStream     int
	AudioCopy       bool
	AudioChannels   int
	AudioSampleRate int
	AudioBitRate    int64
	AudioLanguage   string
	ProgressURL     string
	ManifestURL     string
	Token           string
}

// SessionID is Plex's transcode session id, from the progress URL's
// /video/:/transcode/session/<id>/<uuid>/progress.
func (j Job) SessionID() string {
	u, err := url.Parse(j.ProgressURL)
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

// RingKey picks the worker: the same file with the same audio returns to
// the worker holding its cache.
func (j Job) RingKey() string {
	return fmt.Sprintf("%s|%d|%t|%d|%d|%d|%s", j.Input, j.AudioStream, j.AudioCopy,
		j.AudioChannels, j.AudioSampleRate, j.AudioBitRate, j.SegmentDuration)
}
