package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/time/rate"

	"github.com/mediactl/clusterplex/pkg/hashring"
	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	plexbootstrap "github.com/mediactl/clusterplex/pkg/plex/bootstrap"
	"github.com/mediactl/clusterplex/pkg/plex/epgcloud"
	"github.com/mediactl/clusterplex/pkg/plex/guidebuild"
)

// The cloud guide, built by the managers: plan B of
// docs/superpowers/specs/2026-11-10-guide-writer-and-built-cloud-guide-design.md,
// on with plex.liveTV.cloudGuide "manager".
//
// The lease holder's converger names each block on a Plex cloud lineup
// (Want). When a block's guide is due -- its channel map changed, a day has
// passed, or it was never built -- the holder writes a plan into the shared
// cache: the lineup, the channels, the two weeks. Every serving pod's
// fetcher takes the plan's channels the consistent hash gives it, among the
// serving pods, and keeps their weeks and their shows' and movies' metadata
// fresh in the cache, at its share of the token's rate: every pod calls the
// service with the one server token, so spreading the fetching across pods
// divides one budget rather than adding to it. The holder waits for the
// plan's answers, fetches what is still missing itself, builds the guide
// into a staging schema and swaps it in (plexbootstrap.InstallBuilt).

const (
	// cloudGuideRate is every pod's calls to the guide service together, a
	// second: PMS's own serial load ran at about four without a refusal
	// (talos, 2026-11-04); a burst of eight at once was refused (the rig,
	// 2026-11-10).
	cloudGuideRate = 3.0
	// cloudGridTTL and cloudMetadataTTL are how long an answer is kept:
	// a channel's weeks change through the day; a show's or movie's own
	// metadata hardly ever.
	cloudGridTTL     = 6 * time.Hour
	cloudLineupTTL   = 24 * time.Hour
	cloudMetadataTTL = 7 * 24 * time.Hour
	// cloudBuildEvery is how often a built guide is built again; the build
	// runs in plex.liveTV.guideRefreshWindow when the window allows.
	cloudBuildEvery = 24 * time.Hour
	// cloudFetchWait is how long the holder waits for the other pods'
	// shares before it fetches the rest itself.
	cloudFetchWait = 5 * time.Minute
	// cloudFetchPoll is how often every pod looks at the plans.
	cloudFetchPoll = time.Minute
	// cloudWeeks is how many weeks a build holds, a request each, as PMS's
	// load asks for them.
	cloudWeeks = 2
	planPrefix = "plan/"
)

// cloudPlan is what the holder asks the pods to fetch for one build.
type cloudPlan struct {
	DVR      string    `json:"dvr"`
	Lineup   string    `json:"lineup"`
	Channels []string  `json:"channels"` // grid keys
	Start    time.Time `json:"start"`    // the first day, midnight UTC
}

// weeks are the plan's requests: a week each, both days inclusive.
func (p cloudPlan) weeks() [][2]time.Time {
	out := make([][2]time.Time, cloudWeeks)
	for i := range out {
		s := p.Start.AddDate(0, 0, 7*i)
		out[i] = [2]time.Time{s, s.AddDate(0, 0, 6)}
	}
	return out
}

// cloudWant is a block the converger named.
type cloudWant struct {
	dvr, lineup, mapURI, hash string
}

// cloudBuilt is what was last built for a DVR.
type cloudBuilt struct {
	hash string
	at   time.Time
}

// cloudGuides is the manager's builder of cloud guides.
type cloudGuides struct {
	m     *Manager
	mu    sync.Mutex
	wants map[string]cloudWant // by DVR uuid
	built map[string]cloudBuilt
	kick  chan struct{}
	pace  *pacer
}

// pacer paces the pod's calls at its share of the token's rate, and holds
// them all for the service's Retry-After once it refuses one.
type pacer struct {
	mu      sync.Mutex
	limiter *rate.Limiter
	pods    int
	until   time.Time
}

// Wait implements epgcloud.Limiter.
func (p *pacer) Wait(ctx context.Context) error {
	p.mu.Lock()
	wait := time.Until(p.until)
	p.mu.Unlock()
	if wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return p.limiter.Wait(ctx)
}

// setPods gives the pod its share of the rate among pods.
func (p *pacer) setPods(pods int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pods != p.pods {
		p.pods = pods
		p.limiter.SetLimit(rate.Limit(cloudGuideRate / float64(pods)))
	}
}

// pause holds every call for d.
func (p *pacer) pause(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.until = time.Now().Add(d)
}

func newCloudGuides(m *Manager) *cloudGuides {
	return &cloudGuides{
		m: m, wants: map[string]cloudWant{}, built: map[string]cloudBuilt{}, kick: make(chan struct{}, 1),
		pace: &pacer{limiter: rate.NewLimiter(rate.Limit(cloudGuideRate), 1), pods: 1},
	}
}

// Want implements plexlivetv.CloudGuideBuilder.
func (c *cloudGuides) Want(dvrUUID, lineup, mapURI, lineupHash string) {
	if dvrUUID == "" || lineup == "" {
		return
	}
	c.mu.Lock()
	prev, had := c.wants[dvrUUID]
	c.wants[dvrUUID] = cloudWant{dvr: dvrUUID, lineup: lineup, mapURI: mapURI, hash: lineupHash}
	c.mu.Unlock()
	if !had || prev.hash != lineupHash {
		select {
		case c.kick <- struct{}{}:
		default:
		}
	}
}

func (c *cloudGuides) log() *slog.Logger { return c.m.Logger.With("component", "livetv-cloud-guide") }

// client is the guide service with the server's token, paced by the pod's
// share of the token's rate.
func (c *cloudGuides) client() *epgcloud.Client {
	return &epgcloud.Client{Token: c.m.plexToken(), Limiter: c.pace, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// share sets the pod's share of the rate from how many pods serve, and
// returns the ring that shares the channels out.
func (c *cloudGuides) share(ctx context.Context) *hashring.Ring {
	members := []string{c.m.Config.PodName}
	if others, err := c.m.otherServingPods(ctx); err == nil {
		members = append(members, others...)
	}
	c.pace.setPods(len(members))
	return hashring.New(members...)
}

// runFetcher runs on every pod: it keeps its share of every plan's answers
// fresh in the cache, until ctx ends.
func (c *cloudGuides) runFetcher(ctx context.Context) {
	for {
		if err := c.fetchPlans(ctx, false); err != nil && ctx.Err() == nil {
			c.log().Warn("Live TV: fetch the cloud guides' answers", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(cloudFetchPoll):
		}
	}
}

// fetchPlans fetches this pod's share of every plan, or every plan's whole
// when all is set (the holder, past cloudFetchWait).
func (c *cloudGuides) fetchPlans(ctx context.Context, all bool) error {
	conn, err := c.m.connectGuides(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if err := epgcloud.EnsureCache(ctx, conn); err != nil {
		return err
	}
	plans, err := readPlans(ctx, conn)
	if err != nil {
		return err
	}
	ring := c.share(ctx)
	for _, p := range plans {
		var mine []string
		for _, k := range p.Channels {
			if all || ring.Locate(k) == c.m.Config.PodName {
				mine = append(mine, k)
			}
		}
		if err := c.fetch(ctx, conn, p, mine); err != nil {
			return err
		}
	}
	return nil
}

// fetch keeps channels' weeks, and the metadata of every show and movie
// they name, fresh in the cache.
func (c *cloudGuides) fetch(ctx context.Context, conn *pgx.Conn, p cloudPlan, channels []string) error {
	cl := c.client()
	now := time.Now()
	ids := map[string]bool{}
	for _, k := range channels {
		for _, w := range p.weeks() {
			key := epgcloud.GridKey(k, w[0], w[1])
			body, fresh, err := epgcloud.CacheFresh(ctx, conn, key, cloudGridTTL, now)
			if err != nil {
				return err
			}
			if !fresh {
				if body, err = cl.Grid(ctx, k, w[0], w[1]); err != nil {
					return c.refused(err)
				}
				if err := epgcloud.CachePut(ctx, conn, key, body, time.Now()); err != nil {
					return err
				}
			}
			items, err := epgcloud.DecodeItems(body)
			if err != nil {
				return err
			}
			metadataIDs(items, ids)
		}
	}
	for id := range ids {
		key := epgcloud.MetadataKey(id)
		_, fresh, err := epgcloud.CacheFresh(ctx, conn, key, cloudMetadataTTL, now)
		if err != nil || fresh {
			if err != nil {
				return err
			}
			continue
		}
		body, err := cl.Metadata(ctx, id)
		if errors.Is(err, epgcloud.ErrNotFound) {
			body = []byte(`{"MediaContainer":{}}`)
		} else if err != nil {
			return c.refused(err)
		}
		if err := epgcloud.CachePut(ctx, conn, key, body, time.Now()); err != nil {
			return err
		}
	}
	return nil
}

// metadataIDs adds the rating keys whose own metadata a build reads for
// items: an episode's show's, a movie's own.
func metadataIDs(items []epgcloud.Item, ids map[string]bool) {
	for _, it := range items {
		switch {
		case it.Type == "episode" && it.GrandparentRatingKey != "":
			ids[it.GrandparentRatingKey] = true
		case it.Type == "movie" && it.RatingKey != "":
			ids[it.RatingKey] = true
		}
	}
}

// refused waits out the service's refusal before the error goes back, so
// the next pass does not spend the budget again at once.
func (c *cloudGuides) refused(err error) error {
	var rl *epgcloud.RateLimitedError
	if errors.As(err, &rl) {
		c.pace.pause(max(rl.RetryAfter, time.Minute))
	}
	return err
}

// readPlans is every plan in the cache.
func readPlans(ctx context.Context, conn *pgx.Conn) ([]cloudPlan, error) {
	keys, err := epgcloud.CacheList(ctx, conn, planPrefix)
	if err != nil {
		return nil, err
	}
	var out []cloudPlan
	for k, at := range keys {
		if time.Since(at) > 2*cloudBuildEvery {
			continue // a DVR nobody builds any more
		}
		body, _, ok, err := epgcloud.CacheGet(ctx, conn, k)
		if err != nil || !ok {
			continue
		}
		var p cloudPlan
		if json.Unmarshal(body, &p) == nil && p.Lineup != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// runBuilder runs on the lease holder: it builds each named block's guide
// when it is due, until ctx ends.
func (c *cloudGuides) runBuilder(ctx context.Context) {
	for {
		c.mu.Lock()
		wants := make([]cloudWant, 0, len(c.wants))
		for _, w := range c.wants {
			wants = append(wants, w)
		}
		c.mu.Unlock()
		for _, w := range wants {
			if !c.due(ctx, w) {
				continue
			}
			if err := c.build(ctx, w); err != nil && ctx.Err() == nil {
				c.log().Warn("Live TV: build a cloud guide", "dvr", w.dvr, "error", err)
				c.mu.Lock()
				c.built[w.dvr] = cloudBuilt{hash: "", at: time.Now().Add(-cloudBuildEvery + 15*time.Minute)}
				c.mu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-c.kick:
		case <-time.After(cloudFetchPoll):
		}
	}
}

// due reports whether w's guide wants building: its map changed since the
// last build, it was never built, or a day has passed (in the refresh
// window when there is one).
func (c *cloudGuides) due(ctx context.Context, w cloudWant) bool {
	c.mu.Lock()
	b, ok := c.built[w.dvr]
	c.mu.Unlock()
	if !ok {
		// A start: the guide's own record says when it was built.
		at, err := c.builtAt(ctx, w.dvr)
		if err != nil {
			return false
		}
		b = cloudBuilt{hash: "", at: at}
		if !at.IsZero() {
			b.hash = w.hash // the map it was built from is not recorded; a day's age decides
		}
		c.mu.Lock()
		c.built[w.dvr] = b
		c.mu.Unlock()
	}
	switch {
	case b.at.IsZero(), b.hash != w.hash:
		return true
	case time.Since(b.at) < cloudBuildEvery-time.Hour:
		return false
	}
	window, err := parseWindow(c.m.Config.LiveTV.GuideRefreshWindow)
	if err != nil || window == ([2]int{}) {
		return true
	}
	return inUTCWindow(time.Now().UTC().Hour(), window) || time.Since(b.at) > 2*cloudBuildEvery
}

func inUTCWindow(h int, w [2]int) bool {
	if w[0] < w[1] {
		return h >= w[0] && h < w[1]
	}
	return h >= w[0] || h < w[1]
}

func (c *cloudGuides) builtAt(ctx context.Context, dvr string) (time.Time, error) {
	conn, err := c.m.connectGuides(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	return plexbootstrap.GuideBuiltAt(ctx, conn, plexbootstrap.Guide{Provider: cloudGuideProvider, DVR: dvr})
}

// build plans w's guide, has the pods fetch it, and builds and installs it.
func (c *cloudGuides) build(ctx context.Context, w cloudWant) error {
	log := c.log().With("dvr", w.dvr)
	channels, err := c.mapChannels(ctx, w)
	if err != nil {
		return err
	}
	start := time.Now().UTC().Truncate(24 * time.Hour)
	p := cloudPlan{DVR: w.dvr, Lineup: w.lineup, Channels: channels, Start: start}
	conn, err := c.m.connectGuides(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if err := epgcloud.EnsureCache(ctx, conn); err != nil {
		return err
	}
	body, _ := json.Marshal(p)
	if err := epgcloud.CachePut(ctx, conn, planPrefix+w.dvr, body, time.Now()); err != nil {
		return err
	}
	began := time.Now()
	// This pod's share first, then the other pods' while they fetch, then
	// whatever is left.
	if err := c.fetchPlans(ctx, false); err != nil {
		log.Warn("Live TV: fetch this pod's share of a cloud guide", "error", err)
	}
	deadline := time.Now().Add(cloudFetchWait)
	var meta []string
	for {
		missing, err := c.missing(ctx, conn, p, &meta)
		if err != nil {
			return err
		}
		if missing == 0 {
			break
		}
		if time.Now().After(deadline) {
			log.Info("Live TV: fetching the rest of a cloud guide's answers here", "missing", missing)
			if err := c.fetch(ctx, conn, p, p.Channels); err != nil {
				return fmt.Errorf("fetch the rest: %w", err)
			}
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	in, err := c.input(ctx, conn, p)
	if err != nil {
		return err
	}
	g := plexbootstrap.Guide{Provider: cloudGuideProvider, DVR: w.dvr}
	var stats guidebuild.Stats
	err = plexbootstrap.InstallBuilt(ctx, log, conn, c.m.Config.Postgres.Schema, g, func(ctx context.Context, tx pgx.Tx, staging string) error {
		if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT
			coalesce(max(id) FILTER (WHERE name = 'Movies'), 1), coalesce(max(id) FILTER (WHERE name = 'Shows'), 2),
			coalesce(max(id) FILTER (WHERE name = 'Sports'), 3) FROM %s.library_sections`, pgx.Identifier{staging}.Sanitize())).
			Scan(&in.Sections.Movies, &in.Sections.Shows, &in.Sections.Sports); err != nil {
			return err
		}
		in.Now = time.Now()
		var err error
		stats, err = guidebuild.Build(ctx, tx, staging, in)
		return err
	})
	if err != nil {
		return err
	}
	if err := plexbootstrap.MarkGuideRefreshed(ctx, conn, c.m.Config.Postgres.Schema, w.dvr,
		start.AddDate(0, 0, 7*cloudWeeks), time.Now()); err != nil {
		log.Warn("Live TV: write a built guide's provider row", "error", err)
	}
	c.mu.Lock()
	c.built[w.dvr] = cloudBuilt{hash: w.hash, at: time.Now()}
	c.mu.Unlock()
	// Weeks before this build's are no longer asked for.
	if err := epgcloud.CachePrune(ctx, conn, "grid/", start.AddDate(0, 0, -2)); err != nil {
		log.Warn("Live TV: prune the cloud guides' cache", "error", err)
	}
	log.Info("Live TV: built a cloud guide from Plex's guide service", "channels", stats.Channels, "items", stats.Items,
		"airings", stats.Airings, "took", time.Since(began).Round(time.Second))
	if len(stats.Unmatched) > 0 {
		log.Warn("Live TV: mapped channels the cloud lineup lists no station for were left out of its guide",
			"lineup", w.lineup, "gridKeys", stats.Unmatched)
	}
	return nil
}

// mapChannels reads the block's channels' grid keys from the tuner's map,
// each "<lineup>-<gridKey>".
func (c *cloudGuides) mapChannels(ctx context.Context, w cloudWant) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(w.mapURI, "/")+"/plexmap.json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("read the tuner's map: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read the tuner's map: HTTP %d", resp.StatusCode)
	}
	var m []plexapi.ChannelMapping
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("read the tuner's map: %w", err)
	}
	var keys []string
	for _, e := range m {
		if _, k, ok := strings.Cut(e.ChannelKey, "-"); ok && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("the tuner's map names no channel")
	}
	return keys, nil
}

// missing counts the plan's answers the cache lacks or holds stale: its
// grids, and once they are all in, the metadata of every show and movie
// they name, read from them once into *meta. A pod fetches its channels'
// grids before their metadata, so with grids alone the holder could build
// while another pod still fetched shows, and those shows would have no
// summary or genres.
func (c *cloudGuides) missing(ctx context.Context, conn *pgx.Conn, p cloudPlan, meta *[]string) (int, error) {
	now := time.Now()
	var grids []string
	for _, k := range p.Channels {
		for _, w := range p.weeks() {
			grids = append(grids, epgcloud.GridKey(k, w[0], w[1]))
		}
	}
	if n, err := epgcloud.CacheStale(ctx, conn, grids, cloudGridTTL, now); err != nil || n > 0 {
		return n, err
	}
	if *meta == nil {
		ids := map[string]bool{}
		for _, key := range grids {
			body, _, ok, err := epgcloud.CacheGet(ctx, conn, key)
			if err != nil {
				return 0, err
			}
			if !ok {
				continue
			}
			items, err := epgcloud.DecodeItems(body)
			if err != nil {
				return 0, err
			}
			metadataIDs(items, ids)
		}
		*meta = make([]string, 0, len(ids))
		for id := range ids {
			*meta = append(*meta, epgcloud.MetadataKey(id))
		}
	}
	return epgcloud.CacheStale(ctx, conn, *meta, cloudMetadataTTL, now)
}

// input reads a build's answers from the cache, fetching the lineup when
// it is stale.
func (c *cloudGuides) input(ctx context.Context, conn *pgx.Conn, p cloudPlan) (guidebuild.Input, error) {
	in := guidebuild.Input{Lineup: p.Lineup, Channels: p.Channels, Grids: map[string][][]epgcloud.Item{}, Metadata: map[string]epgcloud.Item{}}
	now := time.Now()
	body, fresh, err := epgcloud.CacheFresh(ctx, conn, epgcloud.LineupKey(p.Lineup), cloudLineupTTL, now)
	if err != nil {
		return in, err
	}
	if !fresh {
		if body, err = c.client().Channels(ctx, p.Lineup); err != nil {
			return in, fmt.Errorf("fetch the lineup's stations: %w", err)
		}
		if err := epgcloud.CachePut(ctx, conn, epgcloud.LineupKey(p.Lineup), body, now); err != nil {
			return in, err
		}
	}
	if in.Stations, err = epgcloud.DecodeChannels(body); err != nil {
		return in, err
	}
	for _, k := range p.Channels {
		for _, w := range p.weeks() {
			body, _, ok, err := epgcloud.CacheGet(ctx, conn, epgcloud.GridKey(k, w[0], w[1]))
			if err != nil {
				return in, err
			}
			var items []epgcloud.Item
			if ok {
				if items, err = epgcloud.DecodeItems(body); err != nil {
					return in, err
				}
			}
			in.Grids[k] = append(in.Grids[k], items)
			for _, it := range items {
				id := it.GrandparentRatingKey
				if it.Type == "movie" {
					id = it.RatingKey
				}
				if _, have := in.Metadata[id]; id == "" || have {
					continue
				}
				if mb, _, ok, err := epgcloud.CacheGet(ctx, conn, epgcloud.MetadataKey(id)); err == nil && ok {
					if ms, err := epgcloud.DecodeItems(mb); err == nil && len(ms) > 0 {
						in.Metadata[id] = ms[0]
					}
				}
			}
		}
	}
	return in, nil
}
