// Package worker serves the Remux gRPC service on the remux pool: the
// cache first, the ffgo pipeline from the first gap.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/mediactl/clusterplex/pkg/remux"
	"github.com/mediactl/clusterplex/pkg/remux/cache"
	"github.com/mediactl/clusterplex/pkg/remux/pipeline"
	"github.com/mediactl/clusterplex/proto/remuxpb"
)

type Server struct {
	remuxpb.UnimplementedRemuxServer
	Cache  *cache.Cache
	Run    func(ctx context.Context, job remux.Job, o pipeline.Options, sink pipeline.Sink) (int, error)
	Now    func() time.Time
	Logger *slog.Logger
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) key(j remux.Job, fi os.FileInfo) cache.Key {
	return cache.Key{
		Input: j.Input, Size: fi.Size(), ModTime: fi.ModTime(), AudioStream: j.AudioStream,
		AudioCopy: j.AudioCopy, Channels: j.AudioChannels, SampleRate: j.AudioSampleRate,
		BitRate: j.AudioBitRate, SegmentDuration: j.SegmentDuration,
	}
}

// session is one job's stream: what was sent and the manifest's timelines.
type session struct {
	sendMu   sync.Mutex // the pipeline reports from two goroutines; grpc-go allows one Send at a time
	srv      *Server
	out      remuxpb.Remux_RemuxServer
	job      remux.Job
	entry    *cache.Entry
	start    time.Time
	sentInit bool
	scales   [2]int32
	info     remux.StreamInfo
	manifest remux.Manifest
	end      time.Duration
}

func (s *Server) Remux(p *remuxpb.Job, out remuxpb.Remux_RemuxServer) error {
	j := remux.JobFromProto(p)
	err := s.remux(out.Context(), j, out)
	msg := ""
	if err != nil {
		msg = err.Error()
		if s.Logger != nil {
			s.Logger.Warn("remux failed", "input", j.Input, "segment", j.SkipToSegment, "error", err)
		}
	}
	return out.Send(&remuxpb.Event{Kind: &remuxpb.Event_Done{Done: &remuxpb.Done{Error: msg}}})
}

func (s *Server) remux(ctx context.Context, j remux.Job, out remuxpb.Remux_RemuxServer) error {
	fi, err := os.Stat(j.Input)
	if err != nil {
		return err
	}
	e, err := s.Cache.Open(s.key(j, fi))
	if err != nil {
		return err
	}
	ss := &session{
		srv: s, out: out, job: j, entry: e, start: s.now(),
		manifest: remux.Manifest{StartNumber: j.SkipToSegment, SegmentDuration: j.SegmentDuration, Start: s.now()},
	}
	n, startAt := j.SkipToSegment, time.Duration(-1)
	if v, a, ts, info, ok := e.Init(); ok {
		if seg, ok, err := e.Segment(n); err != nil {
			return err
		} else if ok && (n == 1 && seg.Start == 0 || n > 1) {
			// A segment from a run that started elsewhere has the start the
			// rule gives only when that run reached it from the same
			// keyframe; a chain from the cache is trusted from n onwards.
			ss.info = info
			ss.manifest.Duration = info.Duration
			if err := ss.sendInit(v, a, ts, info); err != nil {
				return err
			}
			for ok {
				if err := ss.Segment(seg); err != nil {
					return err
				}
				n, startAt = n+1, seg.End
				next, found, err := e.Segment(n)
				if err != nil {
					return err
				}
				ok = found && next.Start == seg.End
				seg = next
			}
			if s.Logger != nil {
				s.Logger.Info("remux served from cache", "input", j.Input, "from", j.SkipToSegment, "to", n-1)
			}
			if last := e.Last(); last > 0 && n > last {
				return ss.finish()
			}
		}
	}
	last, err := s.Run(ctx, j, pipeline.Options{From: n, StartAt: startAt}, ss)
	if err != nil {
		return err
	}
	if err := e.SetLast(last); err != nil {
		return err
	}
	_ = s.Cache.Evict()
	return ss.finish()
}

func (ss *session) send(e *remuxpb.Event) error {
	ss.sendMu.Lock()
	defer ss.sendMu.Unlock()
	return ss.out.Send(e)
}

func (ss *session) file(name string, data []byte) error {
	return ss.send(&remuxpb.Event{Kind: &remuxpb.Event_File{File: &remuxpb.File{Name: name, Data: data}}})
}

func (ss *session) sendInit(v, a []byte, ts [2]int32, info remux.StreamInfo) error {
	if ss.sentInit {
		return nil
	}
	ss.sentInit, ss.scales = true, ts
	ss.manifest.Video = remux.Representation{Codecs: info.VideoCodec, Width: info.Width, Height: info.Height, FrameRate: info.FrameRate, Timescale: ts[0]}
	ss.manifest.Audio = remux.Representation{Codecs: "mp4a.40.2", SampleRate: info.SampleRate, Channels: info.Channels, Language: ss.job.AudioLanguage, Timescale: ts[1]}
	if err := ss.file(remux.InitFile(0), v); err != nil {
		return err
	}
	return ss.file(remux.InitFile(1), a)
}

// Init implements pipeline.Sink: the cache keeps the inits (resetting an
// entry muxed against others), the client gets them once.
func (ss *session) Init(v, a []byte, ts [2]int32, info remux.StreamInfo) error {
	ss.info = info
	if err := ss.entry.SetInit(v, a, ts, info); err != nil {
		return err
	}
	if ss.sentInit && ts != ss.scales {
		return errors.New("remux: the cached inits' timescales differ from this run's")
	}
	ss.manifest.Duration = info.Duration
	return ss.sendInit(v, a, ts, info)
}

// Segment implements pipeline.Sink: into the cache, then to the client,
// files before the manifest that lists them.
func (ss *session) Segment(s remux.Segment) error {
	if err := ss.entry.Put(s); err != nil {
		return err
	}
	if err := ss.file(remux.ChunkFile(0, s.N), s.Video.Data); err != nil {
		return err
	}
	if err := ss.file(remux.ChunkFile(1, s.N), s.Audio.Data); err != nil {
		return err
	}
	ss.manifest.Video.Timeline = append(ss.manifest.Video.Timeline, remux.TimelineEntry{T: s.Video.T, D: s.Video.D})
	ss.manifest.Audio.Timeline = append(ss.manifest.Audio.Timeline, remux.TimelineEntry{T: s.Audio.T, D: s.Audio.D})
	ss.end = s.End
	ss.manifest.Now = ss.srv.now()
	return ss.send(&remuxpb.Event{Kind: &remuxpb.Event_Manifest{Manifest: &remuxpb.Manifest{Mpd: ss.manifest.Render()}}})
}

// Progress implements pipeline.Sink.
func (ss *session) Progress(path, query string) {
	_ = ss.send(&remuxpb.Event{Kind: &remuxpb.Event_Progress{Progress: &remuxpb.Progress{Path: path, Query: query}}})
}

func (ss *session) finish() error {
	ss.manifest.Final = true
	if ss.manifest.Duration == 0 {
		ss.manifest.Duration = ss.end
	}
	if err := ss.send(&remuxpb.Event{Kind: &remuxpb.Event_Manifest{Manifest: &remuxpb.Manifest{Mpd: ss.manifest.Render()}}}); err != nil {
		return fmt.Errorf("final manifest: %w", err)
	}
	return nil
}
