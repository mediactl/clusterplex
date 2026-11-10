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
// into a staging schema and swaps it in: in a build Job by default
// (cloudguidejob.go, plexbootstrap.StageBuilt then SwapStaged), else in its
// own process (plexbootstrap.InstallBuilt). Every pod's calls share one
// budget at cloudGuideRate, kept in PostgreSQL (epgcloud.Budget).

const (
	// cloudGuideRate is every pod's calls to the guide service together, a
	// second: PMS's own serial load ran at about four without a refusal
	// (talos, 2026-11-04); a burst of eight at once was refused (the rig,
	// 2026-11-10). The pods draw on it from one row (cloudBudgetKey).
	cloudGuideRate = 3
	// cloudBudgetKey names the guide service's budget: one server token.
	cloudBudgetKey = "plex-guide-service"
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
	// cloudRetryAfter is the wait after a failed build before the next.
	cloudRetryAfter = 15 * time.Minute
	// cloudProgressEvery is how often a fetch says how far it has got: a
	// cold fetch of 248 channels runs half an hour or more.
	cloudProgressEvery = 2 * time.Minute
	// cloudFetchPoll is how often every pod looks at the plans.
	cloudFetchPoll = time.Minute
	// cloudWeeks is how many weeks a build holds, a request each, as PMS's
	// load asks for them.
	cloudWeeks = 2
	planPrefix = "plan/"
)

// cloudPlan is what the holder asks the pods to fetch for one build.
type cloudPlan struct {
	DVR      string               `json:"dvr"`
	Lineup   string               `json:"lineup"`
	Channels []guidebuild.Channel `json:"channels"` // the map's, grid key and number
	Start    time.Time            `json:"start"`    // the first day, midnight UTC
}

// gridKeys is the grid keys the plan's channels are fetched by, each once.
func (p cloudPlan) gridKeys() []string {
	var out []string
	for _, c := range p.Channels {
		if !slices.Contains(out, c.GridKey) {
			out = append(out, c.GridKey)
		}
	}
	return out
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

// cloudLineupID is a cloud lineup's id, the 24 hex digits the guide service
// knows it by. A block names its lineup as PMS does,
// lineup://tv.plex.providers.epg.cloud/<id>#<title> (clustarr's
// status.blocks[].lineup), which the service answers 404 as a lineup id: on
// talos the first build fetched every grid and show for 52 minutes and then
// failed on the lineup's stations (2026-11-10). A bare id stays as it is.
func cloudLineupID(lineup string) string {
	id := strings.TrimPrefix(lineup, "lineup://tv.plex.providers.epg.cloud/")
	id, _, _ = strings.Cut(id, "#")
	return strings.Trim(id, "/")
}

// cloudWant is a block the converger named.
type cloudWant struct {
	dvr, lineup, mapURI, hash string
}

// cloudBuilt is what was last built for a DVR.
type cloudBuilt struct {
	hash string
	at   time.Time
	// failed is when the last build failed; due waits cloudRetryAfter
	// from it.
	failed time.Time
}

// cloudGuides is the manager's builder of cloud guides.
type cloudGuides struct {
	m     *Manager
	mu    sync.Mutex
	wants map[string]cloudWant // by DVR uuid
	built map[string]cloudBuilt
	kick  chan struct{}
}

func newCloudGuides(m *Manager) *cloudGuides {
	return &cloudGuides{
		m: m, wants: map[string]cloudWant{}, built: map[string]cloudBuilt{}, kick: make(chan struct{}, 1),
	}
}

// Want implements plexlivetv.CloudGuideBuilder.
func (c *cloudGuides) Want(dvrUUID, lineup, mapURI, lineupHash string) {
	lineup = cloudLineupID(lineup)
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

// budget is the guide service's one budget, claimed through q.
func budget(q epgcloud.Querier) *epgcloud.Budget {
	return &epgcloud.Budget{Q: q, Key: cloudBudgetKey, Interval: time.Second / cloudGuideRate}
}

// client is the guide service with the server's token, paced by the
// budget through conn, which the caller uses for nothing else while a call
// waits.
func (c *cloudGuides) client(conn *pgx.Conn) *epgcloud.Client {
	return &epgcloud.Client{Token: c.m.plexToken(), Limiter: budget(conn), HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// share is the ring that shares the channels out among the serving pods.
func (c *cloudGuides) share(ctx context.Context) *hashring.Ring {
	members := []string{c.m.Config.PodName}
	if others, err := c.m.otherServingPods(ctx); err == nil {
		members = append(members, others...)
	}
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
	if err := epgcloud.EnsureBudget(ctx, conn); err != nil {
		return err
	}
	plans, err := readPlans(ctx, conn)
	if err != nil {
		return err
	}
	ring := c.share(ctx)
	for _, p := range plans {
		var mine []string
		for _, k := range p.gridKeys() {
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
	cl := c.client(conn)
	now := time.Now()
	ids := map[string]bool{}
	pr := c.progress(p.DVR)
	for _, k := range channels {
		for _, w := range p.weeks() {
			key := epgcloud.GridKey(k, w[0], w[1])
			body, fresh, err := epgcloud.CacheFresh(ctx, conn, key, cloudGridTTL, now)
			if err != nil {
				return err
			}
			if !fresh {
				if body, err = cl.Grid(ctx, k, w[0], w[1]); err != nil {
					return refused(ctx, conn, err)
				}
				if err := epgcloud.CachePut(ctx, conn, key, body, time.Now()); err != nil {
					return err
				}
				pr.grids++
				pr.say(false)
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
			return refused(ctx, conn, err)
		}
		if err := epgcloud.CachePut(ctx, conn, key, body, time.Now()); err != nil {
			return err
		}
		pr.shows++
		pr.say(false)
	}
	pr.say(true)
	return nil
}

// fetchProgress counts one fetch's calls and says so every
// cloudProgressEvery, and once at its end when it made any.
type fetchProgress struct {
	log          *slog.Logger
	began, said  time.Time
	grids, shows int
}

func (c *cloudGuides) progress(dvr string) *fetchProgress {
	now := time.Now()
	return &fetchProgress{log: c.log().With("dvr", dvr), began: now, said: now}
}

func (f *fetchProgress) say(done bool) {
	if f.grids+f.shows == 0 || (!done && time.Since(f.said) < cloudProgressEvery) {
		return
	}
	f.said = time.Now()
	msg := "Live TV: fetching a cloud guide's answers"
	if done {
		msg = "Live TV: fetched a cloud guide's answers"
	}
	f.log.Info(msg, "grids", f.grids, "shows", f.shows, "took", time.Since(f.began).Round(time.Second).String())
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

// refused holds every pod's calls for the service's Retry-After (a minute
// at least) when err is its refusal, so no pod spends the budget again at
// once, and returns err.
func refused(ctx context.Context, conn *pgx.Conn, err error) error {
	var rl *epgcloud.RateLimitedError
	if errors.As(err, &rl) {
		if herr := budget(conn).Hold(ctx, max(rl.RetryAfter, time.Minute)); herr != nil {
			return errors.Join(err, herr)
		}
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
				c.log().Warn("Live TV: build a cloud guide", "dvr", w.dvr, "error", err,
					"retryIn", cloudRetryAfter.String())
				c.failed(w.dvr, time.Now())
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

// failed records a failed build of dvr's guide at at: the guide is built
// again, but not before cloudRetryAfter (an empty hash alone made every
// failure due again at the next minute's poll: talos's first build, which
// failed on its lineup, retried each minute).
func (c *cloudGuides) failed(dvr string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.built[dvr]
	b.hash, b.failed = "", at
	c.built[dvr] = b
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
		// The map it was built from is not recorded; a day's age decides,
		// unless a build is under way or staged: this holder took over
		// mid-build and builds again, which adopts the Job or the build
		// by the plan's stamp (stageInJob).
		if !at.IsZero() && !c.inFlight(ctx, w.dvr) {
			b.hash = w.hash
		}
		c.mu.Lock()
		c.built[w.dvr] = b
		c.mu.Unlock()
	}
	switch {
	case !b.failed.IsZero() && time.Since(b.failed) < cloudRetryAfter:
		return false
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
	if err := epgcloud.EnsureBudget(ctx, conn); err != nil {
		return err
	}
	body, _ := json.Marshal(p)
	if err := epgcloud.CachePut(ctx, conn, planPrefix+w.dvr, body, time.Now()); err != nil {
		return err
	}
	began := time.Now()
	// The lineup first: a lineup the service does not know fails the build
	// before half an hour of fetching, not after it.
	if err := c.freshLineup(ctx, conn, p.Lineup); err != nil {
		return err
	}
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
			if err := c.fetch(ctx, conn, p, p.gridKeys()); err != nil {
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
	if err := c.freshLineup(ctx, conn, p.Lineup); err != nil { // kept fresh past a long fetch
		return err
	}
	g := plexbootstrap.Guide{Provider: cloudGuideProvider, DVR: w.dvr}
	var stats guidebuild.Stats
	inJob := c.m.Config.LiveTV.CloudGuideBuild != CloudGuideBuildInProcess
	if inJob {
		stamp := cloudPlanStamp(body)
		if err := c.stageInJob(ctx, log, conn, g, stamp); err != nil {
			return err
		}
		if err := plexbootstrap.SwapStaged(ctx, log, conn, c.m.Config.Postgres.Schema, g, stamp); err != nil {
			return err
		}
	} else {
		in, err := cloudInput(ctx, conn, p)
		if err != nil {
			return err
		}
		if err := plexbootstrap.InstallBuilt(ctx, log, conn, c.m.Config.Postgres.Schema, g, cloudBuildFunc(&in, &stats)); err != nil {
			return err
		}
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
	if inJob {
		log.Info("Live TV: built a cloud guide from Plex's guide service in a Job", "took", time.Since(began).Round(time.Second).String())
		return nil
	}
	log.Info("Live TV: built a cloud guide from Plex's guide service", "channels", stats.Channels, "items", stats.Items,
		"airings", stats.Airings, "took", time.Since(began).Round(time.Second).String())
	if len(stats.Unmatched) > 0 {
		log.Warn("Live TV: mapped channels the cloud lineup lists no station for were left out of its guide",
			"lineup", w.lineup, "gridKeys", stats.Unmatched)
	}
	return nil
}

// mapChannels reads the block's channels from the tuner's map: each
// channel key's grid key ("<lineup>-<gridKey>") and its lineupIdentifier,
// the lineup's number for the station, which says which of a station's
// places in the lineup PMS takes (guidebuild.Channel).
func (c *cloudGuides) mapChannels(ctx context.Context, w cloudWant) ([]guidebuild.Channel, error) {
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
	var out []guidebuild.Channel
	for _, e := range m {
		if _, k, ok := strings.Cut(e.ChannelKey, "-"); ok {
			if ch := (guidebuild.Channel{GridKey: k, VCN: e.LineupIdentifier}); !slices.Contains(out, ch) {
				out = append(out, ch)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the tuner's map names no channel")
	}
	return out, nil
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
	for _, k := range p.gridKeys() {
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

// freshLineup fetches lineup's stations into the cache when they are
// stale there, so a build Job, which holds no token, finds them.
func (c *cloudGuides) freshLineup(ctx context.Context, conn *pgx.Conn, lineup string) error {
	_, fresh, err := epgcloud.CacheFresh(ctx, conn, epgcloud.LineupKey(lineup), cloudLineupTTL, time.Now())
	if err != nil || fresh {
		return err
	}
	body, err := c.client(conn).Channels(ctx, lineup)
	if err != nil {
		return fmt.Errorf("fetch the lineup's stations: %w", refused(ctx, conn, err))
	}
	return epgcloud.CachePut(ctx, conn, epgcloud.LineupKey(lineup), body, time.Now())
}

// cachedPlan is the plan cached for dvr, as the holder wrote it.
func cachedPlan(ctx context.Context, conn *pgx.Conn, dvr string) ([]byte, time.Time, bool, error) {
	return epgcloud.CacheGet(ctx, conn, planPrefix+dvr)
}

// cloudInput reads a build's answers from the cache. The lineup's stations
// are taken as cached (freshLineup keeps them), and are an error only when
// none are.
func cloudInput(ctx context.Context, conn *pgx.Conn, p cloudPlan) (guidebuild.Input, error) {
	in := guidebuild.Input{Lineup: p.Lineup, Channels: p.Channels, Grids: map[string][][]epgcloud.Item{}, Metadata: map[string]epgcloud.Item{}}
	body, _, ok, err := epgcloud.CacheGet(ctx, conn, epgcloud.LineupKey(p.Lineup))
	if err != nil {
		return in, err
	}
	if !ok {
		return in, fmt.Errorf("the cache holds no stations for lineup %s", p.Lineup)
	}
	if in.Stations, err = epgcloud.DecodeChannels(body); err != nil {
		return in, err
	}
	for _, k := range p.gridKeys() {
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
