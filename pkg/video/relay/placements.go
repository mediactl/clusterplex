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
	mu sync.Mutex
	m  map[string]Placement
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

func (p *Placements) set(session string, pl Placement) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]Placement{}
	}
	p.m[session] = pl
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

func (p *Placements) remove(session string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, session)
}
