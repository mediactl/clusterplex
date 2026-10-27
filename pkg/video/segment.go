package video

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// KeyframeDue is FFmpeg's -force_key_frames expr:gte(t,n_forced*every): a
// frame at output time t is forced to a keyframe once t reaches forced (the
// keyframes forced so far) times every.
func KeyframeDue(t time.Duration, forced int, every time.Duration) bool {
	return t >= time.Duration(forced)*every
}

// FirstForced is the forced-keyframe count a job starting at start begins
// with: the source's grid of every, so a restart's keyframes fall where a
// run from the start put them. FFmpeg's expression from 0 would force every
// frame until n_forced*every caught up with a restart's -ss; no list Plex's
// transcoder posted after a restart shows such a burst (finding R).
func FirstForced(start, every time.Duration) int {
	if start <= 0 || every <= 0 {
		return 0
	}
	return int(start / every)
}

// SegmentName is segment n's file name under Plex's pattern (media-%05d.ts).
func SegmentName(pattern string, n int) string {
	return fmt.Sprintf(pattern, n)
}

// CutDue is Plex's stream segmenter's rule (finding R): a forced keyframe at
// pts starts the next segment once pts-origin reaches the end of segment
// cut, (cut+1) segment times less the time delta.
func CutDue(pts, origin time.Duration, cut int, s Segmenting) bool {
	return pts-origin >= time.Duration(cut+1)*s.Duration-s.TimeDelta
}

// Span is one stream's time within a segment.
type Span struct {
	Stream     int
	Start, End time.Duration
}

// SegmentTimes is a finished segment as its list line reports it.
type SegmentTimes struct {
	Name       string
	Start, End time.Duration
	Streams    []Span
}

// SegmentListLine is a finished segment's line in Plex's CSV list
// (finding L): name, start and end, then each stream's end, the highest
// stream index first -- the audio's, then the video's, even where the audio
// outlasts the video (burn-image's last segment)
// (-segment_list_separate_stream_times 1).
func SegmentListLine(s SegmentTimes) string {
	spans := slices.Clone(s.Streams)
	slices.SortFunc(spans, func(a, b Span) int { return b.Stream - a.Stream })
	ends := make([]time.Duration, len(spans))
	for i, sp := range spans {
		ends[i] = sp.End
	}
	var b strings.Builder
	b.WriteString(s.Name)
	for _, d := range append([]time.Duration{s.Start, s.End}, ends...) {
		b.WriteString(",")
		b.WriteString(seconds6(d))
	}
	b.WriteString("\n")
	return b.String()
}

// seconds6 is d in seconds with six decimals, as FFmpeg prints a time.
func seconds6(d time.Duration) string {
	us := d.Microseconds()
	sign := ""
	if us < 0 {
		sign, us = "-", -us
	}
	return fmt.Sprintf("%s%d.%06d", sign, us/1_000_000, us%1_000_000)
}

// SegmentList is Plex's segment list as its stream segmenter keeps it
// (finding L): each update is the whole list, at most listSize-1 entries,
// newest last, a segment still being written counted and marked with "#".
type SegmentList struct {
	max   int
	lines []string
}

// NewSegmentList is the list for -segment_list_size listSize; 0 keeps every
// entry.
func NewSegmentList(listSize int) *SegmentList {
	return &SegmentList{max: listSize - 1}
}

func (l *SegmentList) window(keep int) []string {
	if l.max <= 0 || len(l.lines) <= keep {
		return l.lines
	}
	return l.lines[len(l.lines)-keep:]
}

// Finished adds a finished segment and is the list to post.
func (l *SegmentList) Finished(s SegmentTimes) string {
	l.lines = append(l.lines, SegmentListLine(s))
	if l.max > 0 && len(l.lines) > l.max {
		l.lines = l.lines[len(l.lines)-l.max:]
	}
	return strings.Join(l.lines, "")
}

// Opened is the list to post once the next segment, name, starts at start:
// the finished entries that fit beside it, then it as unfinished, every
// time its start (2 plus one per stream).
func (l *SegmentList) Opened(name string, start time.Duration, streams int) string {
	keep := len(l.lines)
	if l.max > 0 {
		keep = l.max - 1
	}
	var b strings.Builder
	for _, line := range l.window(keep) {
		b.WriteString(line)
	}
	b.WriteString("#" + name)
	for range 2 + streams {
		b.WriteString("," + seconds6(start))
	}
	b.WriteString("\n")
	return b.String()
}
