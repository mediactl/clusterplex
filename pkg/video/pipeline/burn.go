package pipeline

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/obinnaokechukwu/ffgo/avcodec"
	"github.com/obinnaokechukwu/ffgo/avutil"

	"github.com/mediactl/clusterplex/pkg/video"
)

// maxSubtitleBytes caps a text subtitle held for burn-in: a film's whole
// track is a few hundred KiB.
const maxSubtitleBytes = 16 << 20

// safeName is what a session may be called to name its directory.
var safeName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// burner draws a job's subtitle into its picture (finding B). A text
// subtitle is held, under a name of the worker's own, in the pod's
// memory-backed InputsDir, and drawn by libass in the video's CPU chain; an
// image subtitle (PGS, VobSub) is decoded from the input's own packets and
// composited onto each frame.
type burner struct {
	dir    string // InputsDir/<session>, removed by close
	filter string // the text subtitle's filter, "" for an image

	// An image subtitle's decoder and the displays it has decoded, oldest
	// first; its canvas is the source video's size.
	index            int
	sd               *ffgo.SubtitleDecoder
	refused          refusals
	shows            []show
	canvasW, canvasH int
	shift            time.Duration
}

// show is one display: its rectangles from start until end (the next
// display's start, or its own end when the stream says one).
type show struct {
	start, end time.Duration
	rects      []ffgo.SubtitleRect
}

// imageCodecs are the bitmap subtitles Plex overlays.
var imageCodecs = map[string]bool{"hdmv_pgs_subtitle": true, "dvd_subtitle": true, "dvb_subtitle": true}

func newBurner(ctx context.Context, job video.Job, in *input, o Options) (*burner, error) {
	b := job.Burn
	if b == nil {
		return nil, nil
	}
	if b.Image {
		return newImageBurner(job, in)
	}
	if !safeName.MatchString(job.Session) || o.InputsDir == "" {
		return nil, fmt.Errorf("%w: a burn-in for session %q", video.ErrShape, job.Session)
	}
	if strings.ContainsAny(b.Style, `'\`) {
		return nil, fmt.Errorf("%w: a subtitle style quoting itself", video.ErrShape)
	}
	if b.Input < 0 || b.Input >= len(job.Inputs) {
		return nil, fmt.Errorf("%w: a burn-in of input %d", video.ErrShape, b.Input)
	}
	dir := filepath.Join(o.InputsDir, job.Session)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	bn := &burner{dir: dir}
	data, ext, err := textSubtitle(ctx, job, in, o)
	if err == nil {
		path := filepath.Join(dir, "subtitle."+ext)
		if err = os.WriteFile(path, data, 0o600); err == nil {
			bn.filter = "subtitles=filename=" + path + ":si=0"
			if o.FontsDir != "" {
				bn.filter += ":fontsdir=" + o.FontsDir
			}
			if b.Style != "" {
				bn.filter += ":force_style='" + b.Style + "'"
			}
			return bn, nil
		}
	}
	bn.close()
	return nil, err
}

// textSubtitle is the subtitle to draw, whole, and its file type: a
// sidecar as the manager streams it, or an embedded track extracted from
// the library file.
func textSubtitle(ctx context.Context, job video.Job, in *input, o Options) ([]byte, string, error) {
	src := job.Inputs[job.Burn.Input]
	if src.Streamed {
		if o.Inputs == nil {
			return nil, "", errors.New("a streamed subtitle without a stream")
		}
		data, err := io.ReadAll(io.LimitReader(o.Inputs(job.Burn.Input), maxSubtitleBytes+1))
		if err != nil {
			return nil, "", fmt.Errorf("read the subtitle: %w", err)
		}
		if len(data) > maxSubtitleBytes {
			return nil, "", fmt.Errorf("%w: a subtitle over %d bytes", video.ErrShape, maxSubtitleBytes)
		}
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(src.Name)), ".")
		switch ext {
		case "srt", "ass", "ssa", "vtt":
		default:
			ext = "srt"
		}
		return data, ext, nil
	}
	if job.Burn.Input != 0 || job.Inputs[0].Streamed {
		return nil, "", fmt.Errorf("%w: an embedded subtitle of a streamed input", video.ErrShape)
	}
	return extracted.get(ctx, job.Inputs[0].Path, job.Burn.Stream, in.shift)
}

// textCache keeps the embedded tracks extracted for burn-in, least
// recently used out, so a seek (Plex restarts the job) does not read the
// whole file again.
type textCache struct {
	mu    sync.Mutex
	max   int
	size  int
	order *list.List // of *cached, most recent first
	byKey map[string]*list.Element
}

type cached struct {
	key  string
	data []byte
	ext  string
}

// extracted is the worker's cache, 64 MiB.
var extracted = &textCache{max: 64 << 20, order: list.New(), byKey: map[string]*list.Element{}}

func (c *textCache) get(ctx context.Context, path string, stream int, shift time.Duration) ([]byte, string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	key := fmt.Sprintf("%s|%d|%d|%d|%d", path, fi.Size(), fi.ModTime().UnixNano(), stream, shift)
	c.mu.Lock()
	if e, ok := c.byKey[key]; ok {
		c.order.MoveToFront(e)
		hit := e.Value.(*cached)
		c.mu.Unlock()
		return hit.data, hit.ext, nil
	}
	c.mu.Unlock()
	data, ext, err := extract(ctx, path, stream, shift)
	if err != nil {
		return nil, "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.byKey[key]; !ok && len(data) <= c.max {
		c.byKey[key] = c.order.PushFront(&cached{key: key, data: data, ext: ext})
		c.size += len(data)
		for c.size > c.max {
			old := c.order.Back()
			c.order.Remove(old)
			o := old.Value.(*cached)
			delete(c.byKey, o.key)
			c.size -= len(o.data)
		}
	}
	return data, ext, nil
}

// textMuxers are the embedded text codecs burned in, by the format their
// track is copied into.
var textMuxers = map[string]string{"ass": "ass", "ssa": "ass", "subrip": "srt", "srt": "srt", "webvtt": "webvtt"}

// extract copies one embedded text track out of path whole, its times
// shifted as the job's frames are (-start_at_zero). It reads the whole
// file: an embedded track is interleaved through it (2026-10-27 ruling).
func extract(ctx context.Context, path string, stream int, shift time.Duration) ([]byte, string, error) {
	d, err := ffgo.NewDecoderWithOptions(path, &ffgo.DecoderOptions{Interrupt: ctx.Done()})
	if err != nil {
		return nil, "", fmt.Errorf("open the subtitle's file: %w", err)
	}
	defer func() { _ = d.Close() }()
	var si *ffgo.StreamInfo
	for _, s := range d.Streams() {
		if s.Index == stream {
			si = s
		}
	}
	if si == nil || si.Type != ffgo.MediaTypeSubtitle {
		return nil, "", fmt.Errorf("%w: stream %d is no subtitle", video.ErrShape, stream)
	}
	format, ok := textMuxers[si.Codec]
	if !ok {
		return nil, "", fmt.Errorf("%w: a %s subtitle burned in", video.ErrShape, si.Codec)
	}
	var out bytes.Buffer
	m, err := ffgo.NewMuxerToWriter(&out, format)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = m.Close() }()
	ms, err := m.AddCopyStream(&ffgo.CopyStreamConfig{CodecParameters: si.CodecParameters(), TimeBase: si.TimeBase})
	if err != nil {
		return nil, "", err
	}
	if err := m.WriteHeader(); err != nil {
		return nil, "", err
	}
	by := int64(0)
	if shift > 0 {
		by = int64(shift) * int64(si.TimeBase.Den) / (int64(si.TimeBase.Num) * int64(time.Second))
	}
	for {
		p, err := d.ReadPacket()
		if err != nil {
			return nil, "", fmt.Errorf("read the subtitle's file: %w", err)
		}
		if p == nil {
			break
		}
		if p.StreamIndex() != stream {
			continue
		}
		if by > 0 {
			if pts := p.PTS(); pts != avutil.AV_NOPTS_VALUE {
				avcodec.SetPacketPTS(p.Raw(), pts-by)
			}
			if dts := p.DTS(); dts != avutil.AV_NOPTS_VALUE {
				avcodec.SetPacketDTS(p.Raw(), dts-by)
			}
		}
		if err := m.WritePacket(ms, p); err != nil {
			return nil, "", err
		}
		if out.Len() > maxSubtitleBytes {
			return nil, "", fmt.Errorf("%w: a subtitle over %d bytes", video.ErrShape, maxSubtitleBytes)
		}
	}
	if err := m.WriteTrailer(); err != nil {
		return nil, "", err
	}
	ext := format
	if ext == "webvtt" {
		ext = "vtt"
	}
	return out.Bytes(), ext, nil
}

func newImageBurner(job video.Job, in *input) (*burner, error) {
	if job.Burn.Input != 0 {
		return nil, fmt.Errorf("%w: an image subtitle of input %d", video.ErrShape, job.Burn.Input)
	}
	var si *ffgo.StreamInfo
	for _, s := range in.d.Streams() {
		if s.Index == job.Burn.Stream {
			si = s
		}
	}
	if si == nil || si.Type != ffgo.MediaTypeSubtitle || !imageCodecs[si.Codec] {
		return nil, fmt.Errorf("%w: stream %d is no image subtitle", video.ErrShape, job.Burn.Stream)
	}
	sd, err := ffgo.NewSubtitleDecoder(si)
	if err != nil {
		return nil, fmt.Errorf("subtitle decoder: %w", err)
	}
	return &burner{index: si.Index, sd: sd, canvasW: in.video.Width, canvasH: in.video.Height, shift: in.shift}, nil
}

// send decodes one of the image subtitle's packets into a display; one the
// decoder refuses is skipped, as ffmpeg(1) skips it.
func (b *burner) send(p *ffgo.Packet) error {
	s, err := b.sd.Decode(p)
	if err != nil {
		return b.refused.refused(err)
	}
	if s == nil {
		return nil
	}
	b.refused.inRow = 0
	at := time.Duration(s.PTS)*time.Microsecond - b.shift
	sh := show{start: at + s.StartTime, end: -1, rects: s.Rects}
	// PGS leaves the end open (UINT32_MAX ms) until the next display.
	if s.EndTime > s.StartTime && s.EndTime < time.Duration(1<<31)*time.Millisecond {
		sh.end = at + s.EndTime
	}
	if n := len(b.shows); n > 0 && (b.shows[n-1].end < 0 || b.shows[n-1].end > sh.start) {
		b.shows[n-1].end = sh.start
	}
	b.shows = append(b.shows, sh)
	return nil
}

// at is what is shown at t, dropping displays over before it.
func (b *burner) at(t time.Duration) []ffgo.SubtitleRect {
	for len(b.shows) > 0 && b.shows[0].end >= 0 && b.shows[0].end <= t {
		b.shows = b.shows[1:]
	}
	if len(b.shows) == 0 || b.shows[0].start > t {
		return nil
	}
	return b.shows[0].rects
}

func (b *burner) close() {
	if b.sd != nil {
		_ = b.sd.Close()
	}
	if b.dir != "" {
		_ = os.RemoveAll(b.dir)
	}
}
