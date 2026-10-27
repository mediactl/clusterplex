package relay

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mediactl/clusterplex/pkg/video"
)

// progressWait is how long the relay waits for Plex's answer to a progress
// report before it carries on: long enough that a throttle answer holds the
// next segment, as it holds Plex's transcoder, and short enough that an
// answer Plex holds stalls nothing.
const progressWait = time.Second

// reporter PUTs a conversion's progress to Plex on a goroutine of its own,
// as Plex's transcoder reports from a thread of its own: Plex answers a
// live DASH session's first report only once the session's first manifest
// is in, which a relay waiting on that answer never posts (talos, rev 114:
// every browser tune stalled until Plex cancelled it). One report is in
// flight at a time, and a newer one replaces a plain report still waiting.
type reporter struct {
	r    *Relay
	job  video.Job
	ctx  context.Context
	wake chan struct{}

	mu       sync.Mutex
	q        []*report
	busy     bool
	throttle string // the throttled report's query, "" when not throttled
	failed   bool

	throttled atomic.Bool
}

type report struct {
	path, query string
	done        chan struct{} // closed once Plex answered it
}

func newReporter(ctx context.Context, r *Relay, job video.Job) *reporter {
	p := &reporter{r: r, job: job, ctx: ctx, wake: make(chan struct{}, 1)}
	go p.run()
	return p
}

// send queues a report. It returns a channel closed once Plex has answered
// it, and whether the reporter was idle, the only case worth waiting on.
func (p *reporter) send(path, query string) (<-chan struct{}, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idle := !p.busy && len(p.q) == 0
	if n := len(p.q); n > 0 && periodic(path, query) && periodic(p.q[n-1].path, p.q[n-1].query) {
		p.q[n-1].query = query
		return p.q[n-1].done, false
	}
	rep := &report{path: path, query: query, done: make(chan struct{})}
	p.q = append(p.q, rep)
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return rep.done, idle
}

// periodic is a progress report the next one makes stale; the input's
// duration and size, and the stream reports, are each sent.
func periodic(path, query string) bool {
	return path == "" && strings.HasPrefix(query, "progress=")
}

// held reports, once, a throttle answer: the query it answered, which
// sloth mode PUTs again.
func (p *reporter) held() (string, bool) {
	if !p.throttled.Swap(false) {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.throttle, true
}

func (p *reporter) run() {
	for {
		p.mu.Lock()
		for len(p.q) == 0 {
			p.mu.Unlock()
			select {
			case <-p.ctx.Done():
				return
			case <-p.wake:
			}
			p.mu.Lock()
		}
		rep := p.q[0]
		p.q = p.q[1:]
		p.busy = true
		path, query := rep.path, rep.query
		p.mu.Unlock()

		// Plex's own transcoder carries on past a failed progress PUT too.
		answer, err := p.r.PMS.Ask(p.ctx, http.MethodPut, p.job.ProgressURL, path, query, http.NoBody, p.job.Token)
		p.mu.Lock()
		p.busy = false
		if err != nil && p.ctx.Err() == nil && !p.failed {
			p.failed = true
			p.r.log().Warn("a progress report failed", "session", p.job.Session, "path", path, "error", err)
		}
		if err == nil && path == "" && canThrottle(answer) {
			p.throttle = query
			p.throttled.Store(true)
		}
		p.mu.Unlock()
		close(rep.done)
	}
}
