// Package epgcloud reads Plex's guide service, https://epg.provider.plex.tv,
// the source PMS loads a cloud (Gracenote) lineup's guide from: the lineup's
// stations, one channel's airings over a run of days, and one show's or
// movie's own metadata. It makes the calls PMS makes (its debug log on the
// kind rig, 2026-11-10) and returns their bodies as they came, for a cache to
// keep; Decode* reads them.
//
// The service limits each server token: about 500 calls in a minute and a
// half were answered, and the next 250, eight at a time, half refused with
// 429 "Rate limit exceeded!" for some minutes after. The caller paces every
// call through Limiter, one budget per token across every pod that uses it.
// docs/superpowers/specs/2026-11-10-guide-writer-and-built-cloud-guide-design.md.
package epgcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// DefaultBaseURL is Plex's guide service.
const DefaultBaseURL = "https://epg.provider.plex.tv"

// maxBody caps one answer: a channel's week is about 0.7 MB, the lineup's
// 1,000 stations about 0.3 MB.
const maxBody = 32 << 20

// ErrResponseTooLarge is an answer over maxBody.
var ErrResponseTooLarge = errors.New("epgcloud: answer over the size cap")

// RateLimitedError is the service's 429: the token's budget is spent.
type RateLimitedError struct {
	// RetryAfter is the service's Retry-After, zero when it gave none.
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("epgcloud: rate limited, retry after %s", e.RetryAfter)
	}
	return "epgcloud: rate limited"
}

// NotFoundError is a 404: an item or lineup the service does not have.
var ErrNotFound = errors.New("epgcloud: not found")

// Limiter paces calls; the caller owns it (one per token).
type Limiter interface {
	Wait(ctx context.Context) error
}

// Client reads the guide service with a server token.
type Client struct {
	BaseURL string // DefaultBaseURL when empty
	Token   string
	HTTP    *http.Client // http.DefaultClient when nil
	Limiter Limiter      // no pacing when nil
	// Language is X-Plex-Language, "en" when empty: PMS sends the DVR's.
	Language string
}

// Channels is GET /lineups/<lineup>/channels: the lineup's stations, at
// most 1,000, in the order PMS numbers its channel tags by.
func (c *Client) Channels(ctx context.Context, lineup string) ([]byte, error) {
	return c.get(ctx, "/lineups/"+url.PathEscape(lineup)+"/channels", nil)
}

// Grid is GET /grid/bulk: one channel's airings from the day start to the
// day end, both inclusive, as PMS asks for a week at a time.
func (c *Client) Grid(ctx context.Context, gridKey string, start, end time.Time) ([]byte, error) {
	q := url.Values{
		"channelGridKey": {gridKey},
		"startDate":      {start.UTC().Format(time.DateOnly)},
		"endDate":        {end.UTC().Format(time.DateOnly)},
	}
	return c.get(ctx, "/grid/bulk", q)
}

// Metadata is GET /library/metadata/<ratingKey>: a show's or a movie's own
// metadata, which PMS reads for every show and movie a grid names.
func (c *Client) Metadata(ctx context.Context, ratingKey string) ([]byte, error) {
	return c.get(ctx, "/library/metadata/"+url.PathEscape(ratingKey), nil)
}

// retryBackoff is the wait before each retry of a request the service
// failed (a 500, 502, 503 or 504, or no answer at all): it fails one now
// and then mid-load (PMS's own refresh on the rig met a 503, 2026-11-10,
// and went on without that answer), and one failed answer would fail a
// whole build, so it is asked again before the build gives up.
var retryBackoff = []time.Duration{2 * time.Second, 5 * time.Second}

// errRetryable marks a failure worth asking again.
type errRetryable struct{ error }

func (e errRetryable) Unwrap() error { return e.error }

func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		body, err := c.getOnce(ctx, path, q)
		var r errRetryable
		if !errors.As(err, &r) || attempt == len(retryBackoff) || ctx.Err() != nil {
			if errors.As(err, &r) {
				err = r.error
			}
			return body, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryBackoff[attempt]):
		}
	}
}

func (c *Client) getOnce(ctx context.Context, path string, q url.Values) ([]byte, error) {
	if c.Limiter != nil {
		if err := c.Limiter.Wait(ctx); err != nil {
			return nil, err
		}
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	if q == nil {
		q = url.Values{}
	}
	lang := c.Language
	if lang == "" {
		lang = "en"
	}
	q.Set("X-Plex-Language", lang)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.Token)
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("epgcloud: GET %s: %w", path, err)
		}
		return nil, errRetryable{fmt.Errorf("epgcloud: GET %s: %w", path, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, errRetryable{fmt.Errorf("epgcloud: GET %s: %w", path, err)}
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("epgcloud: GET %s: %w", path, ErrResponseTooLarge)
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		e := &RateLimitedError{}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			e.RetryAfter = time.Duration(s) * time.Second
		}
		return nil, e
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("epgcloud: GET %s: %w", path, ErrNotFound)
	case resp.StatusCode == http.StatusInternalServerError, resp.StatusCode == http.StatusBadGateway,
		resp.StatusCode == http.StatusServiceUnavailable, resp.StatusCode == http.StatusGatewayTimeout:
		return nil, errRetryable{fmt.Errorf("epgcloud: GET %s: HTTP %d", path, resp.StatusCode)}
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("epgcloud: GET %s: HTTP %d", path, resp.StatusCode)
	}
	return body, nil
}

// Station is one of a lineup's stations.
type Station struct {
	ID        string `json:"id"` // "<lineup>-<gridKey>"
	GridKey   string `json:"gridKey"`
	VCN       string `json:"vcn"`
	CallSign  string `json:"callSign"`
	Title     string `json:"title"`
	Thumb     string `json:"thumb"`
	Hidden    bool   `json:"hidden"`
	IsHD      bool   `json:"isHd"`
	ShortName string `json:"shortTitle"`
	// Affiliate is a local station's network ("ABC" for WEAR-DT), which
	// PMS writes in a channel's title in place of the station's.
	Affiliate string `json:"affiliateCallSign"`
}

// DecodeChannels reads a Channels answer.
func DecodeChannels(body []byte) ([]Station, error) {
	var a struct {
		MediaContainer struct {
			Channel []Station `json:"Channel"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("epgcloud: decode channels: %w", err)
	}
	return a.MediaContainer.Channel, nil
}

// Tag is a genre or a team as the service names it.
type Tag struct {
	ID        string `json:"id"`
	Tag       string `json:"tag"`
	RatingKey string `json:"ratingKey"` // "genre_<id>", "team_<id>": the tag's key in PMS
}

// Image is one of an item's images.
type Image struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// Airing is one airing's slot.
type Airing struct {
	ID              string `json:"id"`
	BeginsAt        int64  `json:"beginsAt"`
	EndsAt          int64  `json:"endsAt"`
	Duration        int64  `json:"duration"`
	Live            bool   `json:"live"`
	OnAir           bool   `json:"onAir"`
	Premiere        bool   `json:"premiere"`
	Origin          string `json:"origin"`
	VideoResolution string `json:"videoResolution"`
}

// Item is a grid's episode or movie, with its airings, or a metadata
// answer's show or movie.
type Item struct {
	Type                  string   `json:"type"` // episode, movie, show
	GUID                  string   `json:"guid"`
	RatingKey             string   `json:"ratingKey"`
	Title                 string   `json:"title"`
	Summary               string   `json:"summary"`
	Duration              int64    `json:"duration"`
	Year                  int      `json:"year"`
	Index                 int      `json:"index"`
	ParentIndex           int      `json:"parentIndex"`
	OriginallyAvailableAt string   `json:"originallyAvailableAt"`
	AddedAt               int64    `json:"addedAt"`
	ContentRating         string   `json:"contentRating"`
	Thumb                 string   `json:"thumb"`
	Art                   string   `json:"art"`
	GrandparentGUID       string   `json:"grandparentGuid"`
	GrandparentRatingKey  string   `json:"grandparentRatingKey"`
	GrandparentTitle      string   `json:"grandparentTitle"`
	GrandparentThumb      string   `json:"grandparentThumb"`
	GrandparentArt        string   `json:"grandparentArt"`
	Genre                 []Tag    `json:"Genre"`
	Team                  []Tag    `json:"Team"`
	Image                 []Image  `json:"Image"`
	Media                 []Airing `json:"Media"`
}

// DecodeItems reads a Grid or a Metadata answer's items.
func DecodeItems(body []byte) ([]Item, error) {
	var a struct {
		MediaContainer struct {
			Metadata []Item `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("epgcloud: decode items: %w", err)
	}
	return a.MediaContainer.Metadata, nil
}
