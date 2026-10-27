package pipeline

import (
	"fmt"
	"time"

	"github.com/mediactl/clusterplex/pkg/video"
)

// progressEvery is how often Plex's transcoder reports its progress
// (finding V: about twice a second).
const progressEvery = 500 * time.Millisecond

// progress reports a run's progress under -progressurl as Plex's
// transcoder does (finding V): progress in percent of the input,
// size -1, the seconds remaining at the current speed, the speed in media
// seconds per second, and whether the GPU decodes.
type progress struct {
	job      video.Job
	duration time.Duration
	hw       int
	sink     Sink
	began    time.Time
	last     time.Time
}

func newProgress(job video.Job, duration time.Duration, gpuDecode bool, sink Sink) *progress {
	p := &progress{job: job, duration: duration, sink: sink, began: time.Now()}
	if gpuDecode {
		p.hw = 1
	}
	return p
}

// start reports what Plex's transcoder reports of its input before any
// progress (finding V; the live capture): its duration in seconds, -1 when
// unknown (Live TV), which Plex waits for before it starts a live session
// ("timed out waiting to find duration for live session"), then its
// picture's size.
func (p *progress) start(width, height int) {
	d := -1.0
	if p.duration > 0 {
		d = p.duration.Seconds()
	}
	p.sink.Progress("", fmt.Sprintf("duration=%.6f", d))
	p.sink.Progress("", fmt.Sprintf("width=%d&height=%d", width, height))
}

// at reports t, the latest video time encoded, at most every progressEvery.
func (p *progress) at(t time.Duration) {
	now := time.Now()
	if now.Sub(p.last) < progressEvery {
		return
	}
	p.last = now
	speed := (t - p.job.Start).Seconds() / max(now.Sub(p.began).Seconds(), 0.001)
	// An input of no known duration (Live TV) is -1.0 done with -1 s to
	// go, as Plex's transcoder reports one; 0.0 and 0 kept Plex from
	// answering a browser's start.mpd for 28 s.
	pct, remaining := -1.0, -1
	if p.duration > 0 {
		pct, remaining = 100*t.Seconds()/p.duration.Seconds(), 0
		if speed > 0 {
			remaining = int(max((p.duration-t).Seconds()/speed, 0))
		}
	}
	p.sink.Progress("", fmt.Sprintf("progress=%.1f&size=-1&remaining=%d&speed=%.1f&vdec_hw_status=%d", pct, remaining, max(speed, 0), p.hw))
}
