// Package codecs has Plex download the audio decoders its transcoder ships
// without before a Live TV tune needs one.
//
// Plex's transcoder carries no AAC, MP2 or MP3 decoder, among others: PMS
// downloads each from plex.tv into its Codecs folder, one folder per
// transcoder build and platform (Codecs/a336ba9-…-linux-aarch64/), when it
// starts a transcode of a file it has analysed and found to need one. A
// Live TV stream is never analysed, so a tune never downloads one, and on a
// folder without the decoder every tune of a channel with that audio fails
// -- the transcoder's "sample rate not set", the client's "Could not tune
// channel". A fresh config volume, a PMS upgrade into a new folder and a
// node of another architecture each start without them (talos's arm64 Plex,
// 2026-10-20).
//
// A Prefetcher asks PMS for what a client would: an HLS transcode of a
// library item with that audio, the video copied and the audio converted.
// It then fetches the transcode's first segment, which the transcoder
// cannot write without the decoder. PMS downloads the decoder itself, for
// this server, as it downloads every other; nothing here ships or copies
// one. E-AC-3 is decoded by Plex's EasyAudioEncoder rather than a
// downloaded library, and the same transcode proves that one runs.
package codecs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

const (
	// RecordName is the file in the Codecs folder that records what was
	// fetched, per platform and for which PMS version. It lives with the
	// codecs so that a new volume starts with neither; a new PMS version,
	// which may keep its codecs in a new folder, fetches again.
	RecordName = ".clusterplex-prefetch.json"
	// candidates is how many library items are tried for one codec: PMS
	// refuses a few files it cannot copy (Hi10P H.264 into HLS), and each
	// refusal costs only a decision.
	candidates = 5
	// defaultFirstSegment bounds the wait for the transcoder's first
	// segment. The download comes first, a few seconds from plex.tv.
	defaultFirstSegment = time.Minute
	stopTimeout         = 10 * time.Second
)

// ErrNotAudioOnly is a decision that would not copy the video and convert
// the audio: no profile may convert video under cluster-plex
// (TranscoderCanOnlyRemuxVideo), and copying the audio fetches nothing.
var ErrNotAudioOnly = errors.New("codecs: PMS would not copy the video and convert the audio")

// Client is the Plex client the transcodes are asked for: Android TV, with
// HEVC appended to its HLS target as its app appends it. That profile
// copies H.264 and HEVC Main 10 alike; Plex Web's copies H.264 alone, and
// the library is being re-encoded to HEVC. Checked against PMS 1.43.4 on
// talos, 2026-10-20.
var Client = plexapi.TranscodeClient{
	Identifier:      "clusterplex-codec-prefetch",
	Product:         "Plex for Android (TV)",
	Version:         "10.0",
	Platform:        "Android",
	PlatformVersion: "14",
	Device:          "cluster-plex",
	DeviceName:      "cluster-plex codec prefetch",
	ProfileExtra:    "append-transcode-target-codec(type=videoProfile&context=streaming&protocol=hls&videoCodec=hevc)",
}

// PMS is the Plex Media Server whose codecs are fetched.
type PMS interface {
	ServerVersion(ctx context.Context) (string, error)
	DecideTranscode(ctx context.Context, t plexapi.AudioTranscode) (plexapi.TranscodeDecision, error)
	StartTranscode(ctx context.Context, t plexapi.AudioTranscode) (string, error)
	FirstSegment(ctx context.Context, t plexapi.AudioTranscode, variant string) (int64, error)
	StopTranscode(ctx context.Context, t plexapi.AudioTranscode) error
}

// Items finds up to n library items whose first audio track is codec.
type Items func(ctx context.Context, codec string, n int) ([]int64, error)

// Platform is the suffix PMS gives the Codecs folder of a Go architecture.
func Platform(goarch string) string {
	switch goarch {
	case "arm64":
		return "linux-aarch64"
	case "amd64":
		return "linux-x86_64"
	}
	return "linux-" + goarch
}

// Prefetcher fetches the decoders of Codecs once per PMS version.
type Prefetcher struct {
	PMS   PMS
	Items Items
	// Dir is Plex's Codecs folder, where the record is kept.
	Dir string
	// Platform is the one PMS fetches for: this pod's (Platform).
	Platform string
	// Codecs are the audio codecs, as Plex names them (aac, ac3, eac3,
	// mp2, mp3), whose decoders are fetched.
	Codecs []string
	Logger *slog.Logger
	// FirstSegment bounds the wait for a transcode's first segment; zero
	// is a minute.
	FirstSegment time.Duration
}

// Result is what one Run did, codec by codec.
type Result struct {
	// Fetched ran a transcode that wrote a segment. Recorded had for this
	// PMS version already. NoItem have no library item whose first audio
	// is that codec, so PMS fetches theirs on first use as before. Failed
	// were tried and did not run.
	Fetched, Recorded, NoItem, Failed []string
}

// Done is whether nothing is left to retry.
func (r Result) Done() bool { return len(r.Failed) == 0 }

// record is RecordName's content, by platform.
type record map[string]platformRecord

type platformRecord struct {
	Version string   `json:"version"`
	Codecs  []string `json:"codecs"`
}

// Run fetches every codec not recorded for this PMS version and platform.
// An error is one the whole run stopped on (PMS not answering); a codec
// that failed is in Result.Failed.
func (p *Prefetcher) Run(ctx context.Context) (Result, error) {
	var res Result
	version, err := p.PMS.ServerVersion(ctx)
	if err != nil {
		return res, fmt.Errorf("read the PMS version: %w", err)
	}
	rec, err := readRecord(p.recordPath())
	if err != nil {
		// Costs only fetching again.
		p.logger().Warn("ignoring an unreadable codec prefetch record", "file", p.recordPath(), "error", err)
		rec = record{}
	}
	pr := rec[p.Platform]
	if pr.Version != version {
		pr = platformRecord{Version: version}
	}
	for _, codec := range p.Codecs {
		if slices.Contains(pr.Codecs, codec) {
			res.Recorded = append(res.Recorded, codec)
			continue
		}
		items, err := p.Items(ctx, codec, candidates)
		if err != nil {
			return res, fmt.Errorf("find a library item with %s audio: %w", codec, err)
		}
		if len(items) == 0 {
			res.NoItem = append(res.NoItem, codec)
			continue
		}
		if err := p.fetch(ctx, items); err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			p.logger().Warn("could not have Plex fetch a decoder", "codec", codec, "platform", p.Platform, "error", err)
			res.Failed = append(res.Failed, codec)
			continue
		}
		pr.Codecs = append(pr.Codecs, codec)
		rec[p.Platform] = pr
		if err := writeRecord(p.recordPath(), rec); err != nil {
			return res, fmt.Errorf("record the fetched %s decoder: %w", codec, err)
		}
		p.logger().Info("Plex has the decoder", "codec", codec, "platform", p.Platform, "version", version)
		res.Fetched = append(res.Fetched, codec)
	}
	return res, nil
}

// fetch runs a transcode from each item in turn until one writes a segment.
func (p *Prefetcher) fetch(ctx context.Context, items []int64) error {
	var errs []error
	for _, item := range items {
		err := p.transcode(ctx, item)
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("item %d: %w", item, err))
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}

// transcode asks for one transcode and fetches its first segment.
func (p *Prefetcher) transcode(ctx context.Context, item int64) error {
	t := plexapi.AudioTranscode{Item: item, Session: session(), Client: Client}
	d, err := p.PMS.DecideTranscode(ctx, t)
	if err != nil {
		return err
	}
	if !d.CopiesVideoConvertsAudio() {
		return fmt.Errorf("%w: %d %s", ErrNotAudioOnly, d.Code, d.Text)
	}
	// From here PMS may hold a session, so it is stopped however this
	// ends -- a cancelled run included, or the transcoder runs on.
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
		defer cancel()
		if err := p.PMS.StopTranscode(sctx, t); err != nil {
			p.logger().Debug("stop the codec prefetch transcode", "session", t.Session, "error", err)
		}
	}()
	variant, err := p.PMS.StartTranscode(ctx, t)
	if err != nil {
		return err
	}
	// The segment is the proof: a transcoder short of the decoder fails
	// before it writes one ("sample rate not set"), whatever the playlist
	// lists.
	sctx, cancel := context.WithTimeout(ctx, orDefault(p.FirstSegment, defaultFirstSegment))
	defer cancel()
	_, err = p.PMS.FirstSegment(sctx, t, variant)
	return err
}

func (p *Prefetcher) recordPath() string { return filepath.Join(p.Dir, RecordName) }

func (p *Prefetcher) logger() *slog.Logger {
	if p.Logger == nil {
		return slog.Default()
	}
	return p.Logger
}

func readRecord(path string) (record, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return record{}, nil
	}
	if err != nil {
		return nil, err
	}
	rec := record{}
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// writeRecord replaces the record whole: a temporary file renamed over it.
func writeRecord(path string, rec record) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), RecordName+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// session names one prefetch transcode, so its stop ends that one alone.
func session() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "clusterplex-codec-prefetch-" + hex.EncodeToString(b[:])
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
