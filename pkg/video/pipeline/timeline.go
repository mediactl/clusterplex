package pipeline

import (
	"strings"
	"time"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/obinnaokechukwu/ffgo/avcodec"
	"github.com/obinnaokechukwu/ffgo/avutil"
)

// Where a timestamp jump is re-anchored: ffmpeg(1)'s dts_delta_threshold
// for a format whose timestamps may jump, its dts_error_threshold for any,
// and how far back a dts may step (AV_TIME_BASE/10).
const (
	liveJump  = 10 * time.Second
	fileJump  = time.Hour
	stepsBack = 100 * time.Millisecond
)

// timeline keeps an MPEG-TS input's timestamps continuous across a jump,
// as ffmpeg(1) does for a format whose timestamps may jump
// (AVFMT_TS_DISCONT) when it is not told -copyts: an IPTV
// upstream's encoder restarting, a splice, or a jump back that libavformat
// reads as a 33-bit wrap, 26.5 h on. Every later packet of every stream is
// shifted by what the jump was, so a jump forward is no frozen picture and
// one back no runaway of copies of one frame. A streamed input's timeline
// (Live TV) is re-anchored on a jump of more than liveJump either way; a
// file's, which a seek and Plex's segment list follow, only on a step back
// or a jump of more than fileJump, so a recording's real gap keeps its time.
// Another container's timestamps do not jump, and only a packet's dts is
// read: a Matroska packet's pts steps back at every B-frame.
type timeline struct {
	on     bool
	live   bool
	offset time.Duration         // added to every timestamp
	tb     map[int]ffgo.Rational // each stream's time base
	next   map[int]time.Duration // each stream's next expected dts
	last   map[int]time.Duration // each stream's last dts
}

func newTimeline(in *input, live bool) *timeline {
	tl := &timeline{
		on: strings.Contains(in.d.FormatName(), "mpegts"), live: live,
		tb: map[int]ffgo.Rational{}, next: map[int]time.Duration{}, last: map[int]time.Duration{},
	}
	for _, s := range in.d.Streams() {
		tl.tb[s.Index] = s.TimeBase
	}
	return tl
}

// fix shifts p by the offset so far, and re-anchors on a jump.
func (tl *timeline) fix(p *ffgo.Packet) {
	if !tl.on {
		return
	}
	idx := p.StreamIndex()
	tb, ok := tl.tb[idx]
	if !ok || tb.Num == 0 || tb.Den == 0 {
		return
	}
	shift(p, ticks(tl.offset, tb))
	dts := p.DTS()
	if dts == avutil.AV_NOPTS_VALUE {
		return
	}
	t := toTime(dts, tb)
	if next, ok := tl.next[idx]; ok {
		if delta := t - next; tl.jumped(delta, t, tl.last[idx]) {
			tl.offset -= delta
			shift(p, -ticks(delta, tb))
			t -= delta
		}
	}
	tl.last[idx] = t
	tl.next[idx] = t + toTime(max(avcodec.GetPacketDuration(p.Raw()), 0), tb)
}

func (tl *timeline) jumped(delta, t, last time.Duration) bool {
	switch {
	case t+stepsBack < last:
		return true
	case tl.live:
		return delta > liveJump || delta < -liveJump
	default:
		return delta > fileJump
	}
}

// ticks is d in tb.
func ticks(d time.Duration, tb ffgo.Rational) int64 {
	return int64(float64(d) / float64(time.Second) * float64(tb.Den) / float64(tb.Num))
}

// shift moves p's pts and dts by n ticks.
func shift(p *ffgo.Packet, n int64) {
	if n == 0 {
		return
	}
	if v := p.PTS(); v != avutil.AV_NOPTS_VALUE {
		avcodec.SetPacketPTS(p.Raw(), v+n)
	}
	if v := p.DTS(); v != avutil.AV_NOPTS_VALUE {
		avcodec.SetPacketDTS(p.Raw(), v+n)
	}
}
