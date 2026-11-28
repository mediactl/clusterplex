package plexposters

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// What a request would have met: the counter's request label.
const (
	// metPaired is a row showing clustarr's poster whose compact twin exists.
	metPaired = "paired"
	// metUnpaired is a row showing a poster Plex downloaded with no twin.
	metUnpaired = "unpaired"
	// metCloud is a row stuck on its match result's thumb, served by
	// Plex's cloud photo service.
	metCloud = "cloud"
	// metOther is a poster the user picked or uploaded, or a row that is
	// not there.
	metOther = "other"
	// metUnread is a row that could not be read in time.
	metUnread = "unread"
	// metDropped is a request the observer had no room to look at.
	metDropped = "dropped"
	// metNotPoster begins the label of a request for another image, ended
	// by what that image is (TargetLocal, TargetPlexCloud, TargetPublic,
	// TargetNone).
	metNotPoster = "not-poster-"
)

const (
	// QueueSize is how many requests wait to be looked at; one more is
	// counted as dropped.
	QueueSize = 512
	// lookupTimeout bounds reading one row.
	lookupTimeout = 2 * time.Second
	// maxSamples is how many distinct kinds of request are logged.
	maxSamples = 256
	// maxProduct bounds a client's product name in a log line.
	maxProduct = 48

	downloadedPoster = "metadata://posters/"
	cloudThumb       = "https://" + plexCloudHost + "/"
)

var errNoLogger = errors.New("plexposters: Logger is required")

// Observer watches the poster requests offered to the proxy and answers
// none of them. Divert is on every such request's way to Plex, so it only
// parses and queues; Run reads the rows and counts.
type Observer struct {
	// Thumb is a row's user_thumb_url, empty when the row is not there.
	// Nil counts every poster request as unread.
	Thumb func(ctx context.Context, id int64) (string, error)
	// Paired reports whether the downloaded poster of that name (the SHA-1
	// of the URL it came from, as Plex names it) has a compact twin. Nil is
	// no twin for any.
	Paired  func(name string) bool
	Metrics *Metrics
	Logger  *slog.Logger

	once  sync.Once
	queue chan Request

	seenMu sync.Mutex
	seen   map[sample]struct{}
}

// sample is what makes two requests the same kind in the log.
type sample struct {
	platform, product, platformIn, tokenIn, minSize, upscale, met string
	width, height                                                 int
	tls                                                           bool
}

func (o *Observer) init() {
	o.once.Do(func() {
		o.queue = make(chan Request, QueueSize)
		o.seen = map[sample]struct{}{}
	})
}

// Divert is the proxy's Divert: it claims nothing. A request to the photo
// transcoder is queued for Run, or counted as dropped when the queue is full.
func (o *Observer) Divert(_ context.Context, req *http.Request) (*http.Response, bool) {
	r, ok := Parse(req)
	if !ok {
		return nil, false
	}
	o.init()
	select {
	case o.queue <- r:
	default:
		o.Metrics.count(r, metDropped)
	}
	return nil, false
}

// Run looks at the queued requests until ctx ends.
func (o *Observer) Run(ctx context.Context) error {
	if o.Logger == nil {
		return errNoLogger
	}
	o.init()
	for {
		select {
		case <-ctx.Done():
			return nil
		case r := <-o.queue:
			met := o.met(ctx, r)
			o.Metrics.count(r, met)
			o.log(r, met)
		}
	}
}

// met says what r would have met.
func (o *Observer) met(ctx context.Context, r Request) string {
	if r.Target != TargetPoster {
		return metNotPoster + r.Target
	}
	if o.Thumb == nil {
		return metUnread
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	thumb, err := o.Thumb(ctx, r.Item)
	switch {
	case err != nil:
		return metUnread
	case strings.HasPrefix(thumb, cloudThumb):
		return metCloud
	case !strings.HasPrefix(thumb, downloadedPoster):
		return metOther
	}
	// metadata://posters/<agent>_<SHA-1 of the URL it was downloaded from>
	if i := strings.LastIndexByte(thumb, '_'); i >= 0 && o.Paired != nil && o.Paired(thumb[i+1:]) {
		return metPaired
	}
	return metUnpaired
}

// log writes one line for each distinct kind of request, up to maxSamples:
// what a client sends, never its token, a title or an item's key.
func (o *Observer) log(r Request, met string) {
	product := r.Product
	if len(product) > maxProduct {
		product = product[:maxProduct]
	}
	s := sample{
		platform: r.Platform, product: product, platformIn: r.PlatformIn, tokenIn: r.TokenIn,
		minSize: r.MinSize, upscale: r.Upscale, met: met, width: r.Width, height: r.Height, tls: r.TLS,
	}
	o.seenMu.Lock()
	_, seen := o.seen[s]
	if !seen && len(o.seen) < maxSamples {
		o.seen[s] = struct{}{}
	} else {
		seen = true
	}
	o.seenMu.Unlock()
	if seen {
		return
	}
	o.Logger.Info("poster request seen",
		"platform", r.Platform, "product", product, "platform_in", r.PlatformIn, "token_in", r.TokenIn,
		"width", r.Width, "height", r.Height, "min_size", r.MinSize, "upscale", r.Upscale,
		"tls", r.TLS, "request", met, "params", strings.Join(r.Params, ","))
}
