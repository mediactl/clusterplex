package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mediactl/clusterplex/pkg/video"
	"github.com/mediactl/clusterplex/proto/videopb"
)

// maxPlaylistBytes caps a Live TV session's playlist: a line pair a
// second of the session, about 60 bytes, so 4 MiB is most of a day.
const maxPlaylistBytes = 4 << 20

// DefaultLivePoll is how long the follower waits before reading a Live TV
// playlist that named no new segment. Plex lists the segment still being
// written (X-Plex-Incomplete-Segments=1), so a wait is rare.
const DefaultLivePoll = 250 * time.Millisecond

// playlist is an HLS media playlist as Plex serves a Live TV session's
// (finding I): 1 s MPEG-TS segments, numbered from seq.
type playlist struct {
	seq      int64    // #EXT-X-MEDIA-SEQUENCE: the first segment's number
	segments []string // their URIs, in order
	ended    bool     // #EXT-X-ENDLIST
}

func parsePlaylist(b []byte) (playlist, error) {
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	if strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return playlist{}, errors.New("live: not an HLS playlist")
	}
	var pl playlist
	for _, l := range lines[1:] {
		l = strings.TrimSpace(l)
		switch {
		case l == "":
		case strings.HasPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"):
			n, err := strconv.ParseInt(strings.TrimPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
			if err != nil || n < 0 {
				return playlist{}, fmt.Errorf("live: %q", l)
			}
			pl.seq = n
		case l == "#EXT-X-ENDLIST":
			pl.ended = true
		case strings.HasPrefix(l, "#EXT-X-STREAM-INF"), strings.HasPrefix(l, "#EXT-X-MAP"),
			strings.HasPrefix(l, "#EXT-X-KEY") && !strings.Contains(l, "METHOD=NONE"):
			// A master playlist, fMP4 or encrypted segments: no capture
			// of Plex's carries them.
			return playlist{}, fmt.Errorf("live: a playlist with %q", strings.SplitN(l, ":", 2)[0])
		case strings.HasPrefix(l, "#"):
		default:
			pl.segments = append(pl.segments, l)
		}
	}
	return pl, nil
}

func (r *Relay) livePoll() time.Duration {
	if r.LivePoll <= 0 {
		return DefaultLivePoll
	}
	return r.LivePoll
}

// followLive streams a Live TV session's playlist to the worker as one
// MPEG-TS, as FFmpeg's HLS demuxer reads it for Plex's transcoder
// (-live_start_index 0): every segment from the playlist's first, in
// order, each read to its end -- Plex streams the one still being written
// until it is complete -- and the playlist read again for the next, until
// it ends. A segment that left the playlist before it was read is passed
// over, as FFmpeg's demuxer skips ahead.
func (c *conversion) followLive(ctx context.Context, r *Relay, job video.Job, i int32, in video.Input) error {
	base, err := url.Parse(in.Source)
	if err != nil {
		return err
	}
	next := int64(-1)
	for {
		pl, err := r.livePlaylist(ctx, in.Source)
		if err != nil {
			return err
		}
		next = max(next, pl.seq)
		if k := next - pl.seq; k < int64(len(pl.segments)) {
			for _, s := range pl.segments[k:] {
				if err := c.liveSegment(ctx, r, i, base, s); err != nil {
					again, aerr := r.livePlaylist(ctx, in.Source)
					if aerr != nil || next >= again.seq {
						return fmt.Errorf("live segment %d: %w", next, err)
					}
					break // it left the playlist: skip ahead
				}
				next++
			}
			continue
		}
		if pl.ended {
			return c.send(&videopb.Up{Kind: &videopb.Up_Input{Input: &videopb.Chunk{Input: i, Eof: true}}})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.livePoll()):
		}
	}
}

// livePlaylist reads the session's playlist as Plex's transcoder does
// (plexsession.OpenAsTranscoder): Plex serves a live session to no other
// client of a session that converts.
func (r *Relay) livePlaylist(ctx context.Context, src string) (playlist, error) {
	rc, err := r.PMS.OpenAsTranscoder(ctx, src)
	if err != nil {
		return playlist{}, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, maxPlaylistBytes+1))
	if err != nil {
		return playlist{}, fmt.Errorf("live playlist: %w", err)
	}
	if len(b) > maxPlaylistBytes {
		return playlist{}, fmt.Errorf("live playlist over %d bytes", maxPlaylistBytes)
	}
	return parsePlaylist(b)
}

// liveSegment streams one segment, which must be MPEG-TS, to input i.
func (c *conversion) liveSegment(ctx context.Context, r *Relay, i int32, base *url.URL, uri string) error {
	u, err := base.Parse(uri)
	if err != nil {
		return err
	}
	rc, err := r.PMS.OpenAsTranscoder(ctx, u.String())
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
	defer stop()
	defer func() { _ = rc.Close() }()
	buf := make([]byte, inputChunk)
	first := true
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if first && buf[0] != 0x47 {
				return errors.New("a live segment that is not MPEG-TS")
			}
			first = false
			if serr := c.send(&videopb.Up{Kind: &videopb.Up_Input{Input: &videopb.Chunk{Input: i, Data: slices.Clone(buf[:n])}}}); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
