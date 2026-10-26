// Package worker is the GPU pool's gRPC server: one conversion per slot,
// each run by the pipeline in process, its inputs streamed in and its
// output streamed back (cmd/video-worker).
package worker

import (
	"context"
	"fmt"
	"sync"

	"github.com/mediactl/clusterplex/pkg/video"
)

// Slots counts a pod's conversions. A whole-file job (a download, an
// Optimize, a DVR conversion) never takes the last free slot, which is held
// for a viewer's stream; and a job for a session that holds a slot takes it
// over, cancelling the old job: Plex restarts a session on a seek, often
// before the old job has let go.
type Slots struct {
	mu   sync.Mutex
	max  int
	next uint64
	held map[string]held
}

type held struct {
	id     uint64
	cancel context.CancelFunc
}

// NewSlots is max slots, all free.
func NewSlots(max int) *Slots { return &Slots{max: max, held: map[string]held{}} }

// Acquire takes a slot for session's job of kind, which cancel stops if a
// later job of the same session takes the slot over. release frees it; only
// its first call counts, and only while the slot is still this job's.
func (s *Slots) Acquire(session string, kind video.OutputKind, cancel context.CancelFunc) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.held[session]; ok && session != "" {
		old.cancel()
	} else {
		need := 1
		if kind == video.OutputFile {
			need = 2
		}
		if s.max-len(s.held) < need {
			return func() {}, false
		}
	}
	s.next++
	id := s.next
	key := session
	if key == "" {
		key = fmt.Sprintf("\x00%d", id) // a job with no session never shares a slot
	}
	s.held[key] = held{id: id, cancel: cancel}
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if h, ok := s.held[key]; ok && h.id == id {
				delete(s.held, key)
			}
		})
	}, true
}

// Free is how many slots are free.
func (s *Slots) Free() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.max - len(s.held)
}
