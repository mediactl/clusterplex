package pipeline

import (
	"context"
	"sync"
	"time"
)

// boundaries is what the video side has learnt of segment starts, shared
// with the audio muxer, which may hold an encoded packet until the video
// has read past its time: an audio packet is demuxed ahead of the keyframe
// that ends its segment (review focus 2), so cutting audio on its own
// clock would cut early.
type boundaries struct {
	mu      sync.Mutex
	cond    *sync.Cond
	starts  []time.Duration // starts[i] begins segment From+i
	horizon time.Duration   // latest video time read
	done    bool            // the video ended; no boundary will come
}

func newBoundaries() *boundaries {
	b := &boundaries{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *boundaries) add(t time.Duration) {
	b.mu.Lock()
	b.starts = append(b.starts, t)
	b.mu.Unlock()
	b.cond.Broadcast()
}

func (b *boundaries) advance(t time.Duration) {
	b.mu.Lock()
	if t > b.horizon {
		b.horizon = t
	}
	b.mu.Unlock()
	b.cond.Broadcast()
}

func (b *boundaries) finish() {
	b.mu.Lock()
	b.done = true
	b.mu.Unlock()
	b.cond.Broadcast()
}

// wait blocks until the video has read past t, or ended, or ctx ends. It
// returns the starts known then.
func (b *boundaries) wait(ctx context.Context, t time.Duration) ([]time.Duration, error) {
	stop := context.AfterFunc(ctx, b.cond.Broadcast)
	defer stop()
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.horizon < t && !b.done {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.cond.Wait()
	}
	return append([]time.Duration(nil), b.starts...), ctx.Err()
}
