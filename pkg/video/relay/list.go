package relay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mediactl/clusterplex/pkg/plexsession"
	"github.com/mediactl/clusterplex/pkg/video"
)

// lister tells Plex of each segment as its stream segmenter does (finding
// L): after a segment's file is in place, the whole finished list, then
// again with the next segment listed unfinished, each a POST of its own,
// chunked, to the job's -segment_list URL.
type lister struct {
	pms   plexsession.PMS
	job   video.Job
	list  *video.SegmentList
	next  int // the number the next segment must carry
	count int
}

func newLister(pms plexsession.PMS, job video.Job) *lister {
	return &lister{pms: pms, job: job, list: video.NewSegmentList(job.Segments.ListSize), next: job.Segments.StartNumber}
}

// name is the next segment's expected name: a worker names segments in
// order from -segment_start_number, or the job fails.
func (l *lister) want() string { return video.SegmentName(l.job.Segments.Pattern, l.next) }

// Finished lists segment s, landed; nextStart, when the worker has opened
// the next segment, lists that one unfinished too.
func (l *lister) Finished(ctx context.Context, s video.SegmentTimes, nextStart *time.Duration) error {
	if s.Name != l.want() {
		return fmt.Errorf("segment %q out of order: want %q", s.Name, l.want())
	}
	l.next++
	l.count++
	if err := l.post(ctx, l.list.Finished(s)); err != nil {
		return err
	}
	if nextStart == nil {
		return nil
	}
	return l.post(ctx, l.list.Opened(l.want(), *nextStart, len(s.Streams)))
}

// post sends one list. The body's length is withheld, so it goes chunked
// as Plex's transcoder sends it.
func (l *lister) post(ctx context.Context, body string) error {
	return l.pms.Send(ctx, http.MethodPost, l.job.SegmentListURL, "", "", io.MultiReader(strings.NewReader(body)), l.job.Token)
}

// progressPaths and progressKeys are what Plex's transcoder sends under its
// progress URL: the remux pool's DASH recordings (2026-10-01) and a video
// conversion's (finding V: vdec_hw_status).
var (
	progressPaths = map[string]bool{"": true, "stream": true, "streamDetail": true}
	progressKeys  = map[string]bool{
		"progress": true, "size": true, "remaining": true, "speed": true, "duration": true,
		"vdec_hw_status": true,
		"width":          true, "height": true, "index": true, "id": true, "codec": true, "type": true,
		"profile": true, "language": true, "channels": true, "layout": true, "sampleRate": true,
		"level": true, "frameRate": true, "interlaced": true, "sar": true,
	}
)

// progressAllowed reports whether a worker's progress report is one of
// Plex's: the pool parses untrusted media, and the relay sends these with
// the session's token, so nothing else may be steered through it.
func progressAllowed(path, query string) bool {
	if !progressPaths[path] {
		return false
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		return false
	}
	for k := range q {
		if !progressKeys[k] {
			return false
		}
	}
	return true
}
