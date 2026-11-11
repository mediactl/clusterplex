package relay

import "sync"

// Placement is where one conversion runs: the pool's pod and its node from
// the start, the encoder and decode device once the worker reports them
// (videopb.Started; empty until it has, and from a worker that predates it).
type Placement struct {
	Worker, Node string
	// Pool is the pool the conversion runs on: PoolGPU or PoolCPU.
	Pool string
	// Encoder is FFmpeg's ("h264_qsv"); Decoder the device the video
	// decodes on ("vaapi"), empty for the CPU.
	Encoder, Decoder string
}

// Placements are the running conversions' placements, by Plex's transcode
// session. Plex reports every conversion as its own, run in software on its
// node; the manager's activity monitor labels the session with this instead.
// A nil *Placements records nothing.
type Placements struct {
	mu  sync.Mutex
	m   map[string]Placement
	ids map[string]uint64 // each entry's own id, so a run removes only what it set
	seq uint64
}

// Get is session's placement while its conversion runs.
func (p *Placements) Get(session string) (Placement, bool) {
	if p == nil {
		return Placement{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pl, ok := p.m[session]
	return pl, ok
}

// set records session's placement, replacing an earlier run's (PMS starts
// a seek's new transcoder before the old one has let go), and returns the
// entry's id for remove: the old run's end must not remove the new run's.
func (p *Placements) set(session string, pl Placement) uint64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m, p.ids = map[string]Placement{}, map[string]uint64{}
	}
	p.seq++
	p.m[session], p.ids[session] = pl, p.seq
	return p.seq
}

func (p *Placements) started(session, encoder, decoder string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if pl, ok := p.m[session]; ok {
		pl.Encoder, pl.Decoder = encoder, decoder
		p.m[session] = pl
	}
}

// remove forgets session's placement when it is still the one set
// returned id for; a later run's placement stays.
func (p *Placements) remove(session string, id uint64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ids[session] != id {
		return
	}
	delete(p.m, session)
	delete(p.ids, session)
}
