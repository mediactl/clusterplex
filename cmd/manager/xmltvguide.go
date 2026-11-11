package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	plexbootstrap "github.com/mediactl/clusterplex/pkg/plex/bootstrap"
	"github.com/mediactl/clusterplex/pkg/plex/epgcloud"
	"github.com/mediactl/clusterplex/pkg/plex/guidebuild"
	"github.com/mediactl/clusterplex/pkg/plex/xmltvguide"
)

// The XMLTV guides, built by the managers: plex.liveTV.xmltvGuide "manager"
// (clustarr's docs/superpowers/specs/2026-11-10-livetv-custom-guides-design.md
// §5.8).
//
// PMS's own load at a DVR's creation is its guide's first fill: it is what
// lets PMS pair each of the device's numbers with its XMLTV channel, and the
// converger saves the channel map once it has. From then on the lease
// holder's converger names the block on every pass (Want) and the guide is
// the managers': the holder reads the device's lineup.json and its XMLTV
// from the tuner (the URL PMS loads), caches the XMLTV in the shared cache,
// writes a plan beside the cloud guides' and has the guide built from them
// and swapped in -- in a build Job by default, as a cloud guide is
// (cloudguidejob.go), else in its own process. PMS's own loads of the guide
// are cancelled after a map save (the converger) and refused at their
// rename (loadsNotOurs), and its writes to it are held (the writer file).

const (
	// xmltvBuildFloor is the least time between two builds of a guide on a
	// hash move: clustarr renders a block's XMLTV again on every change of a
	// guide source, so the hash can move many times an hour.
	xmltvBuildFloor = 30 * time.Minute
	// xmltvBuildEvery is how often a guide whose XMLTV did not change is
	// built again, so airings that ended leave it.
	xmltvBuildEvery = 24 * time.Hour
	// xmltvMaxBody caps a block's XMLTV as fetched (a week of 450 channels
	// is about 20 MB), and xmltvMaxLineup its lineup.json.
	xmltvMaxBody   = 64 << 20
	xmltvMaxLineup = 1 << 20
	// xmltvFetchTimeout bounds a fetch from the tuner, in the cluster.
	xmltvFetchTimeout = 5 * time.Minute
	// xmltvPrefix keys the cached XMLTV bodies, xmltv/<dvr>/<hash>.
	xmltvPrefix = "xmltv/"
)

// xmltvPlan is what the holder caches for one XMLTV build, at plan/<dvr>
// beside the cloud plans: the XMLTV at xmltv/<dvr>/<hash>, and the device's
// numbers by XMLTV channel id. Its Provider tells runGuideBuild it is no
// cloud plan.
type xmltvPlan struct {
	Provider string            `json:"provider"`
	DVR      string            `json:"dvr"`
	URL      string            `json:"url"`
	Hash     string            `json:"hash"`
	Lineup   map[string]string `json:"lineup"` // channel id -> GuideNumber
}

// xmltvKey is the cache key of a DVR's XMLTV at a hash.
func xmltvKey(dvr, hash string) string { return xmltvPrefix + dvr + "/" + hash }

// planProvider is the guide provider a cached plan is for: its "provider",
// or a cloud lineup's for a plan that names none (every cloud plan).
func planProvider(body []byte) string {
	var p struct {
		Provider string `json:"provider"`
	}
	if json.Unmarshal(body, &p) != nil || p.Provider == "" {
		return cloudGuideProvider
	}
	return p.Provider
}

// xmltvWant is a block the converger named.
type xmltvWant struct {
	dvr, url, hash string
}

// xmltvGuides is the manager's builder of XMLTV guides.
type xmltvGuides struct {
	m     *Manager
	mu    sync.Mutex
	wants map[string]xmltvWant // by DVR uuid, lower case
	built map[string]cloudBuilt
	kick  chan struct{}
}

func newXMLTVGuides(m *Manager) *xmltvGuides {
	return &xmltvGuides{m: m, wants: map[string]xmltvWant{}, built: map[string]cloudBuilt{}, kick: make(chan struct{}, 1)}
}

// Want implements plexlivetv.XMLTVGuideBuilder.
func (x *xmltvGuides) Want(dvrUUID, guideURL, guideHash string) {
	if dvrUUID == "" || guideURL == "" {
		return
	}
	k := strings.ToLower(dvrUUID)
	x.mu.Lock()
	prev, had := x.wants[k]
	x.wants[k] = xmltvWant{dvr: dvrUUID, url: guideURL, hash: guideHash}
	x.mu.Unlock()
	if !had || prev.hash != guideHash || prev.url != guideURL {
		select {
		case x.kick <- struct{}{}:
		default:
		}
	}
}

// wanted reports whether the converger has named dvr's guide as the
// managers': PMS's loads of it are refused from then on, never before, so
// the first load, which pairs the channel map, is PMS's. Nil wants none.
func (x *xmltvGuides) wanted(dvr string) bool {
	if x == nil {
		return false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	_, ok := x.wants[strings.ToLower(dvr)]
	return ok
}

func (x *xmltvGuides) log() *slog.Logger { return x.m.Logger.With("component", "livetv-xmltv-guide") }

// runBuilder runs on the lease holder: it builds each named block's guide
// when it is due, until ctx ends.
func (x *xmltvGuides) runBuilder(ctx context.Context) {
	for {
		x.mu.Lock()
		wants := make([]xmltvWant, 0, len(x.wants))
		for _, w := range x.wants {
			wants = append(wants, w)
		}
		x.mu.Unlock()
		slices.SortFunc(wants, func(a, b xmltvWant) int { return strings.Compare(a.dvr, b.dvr) })
		for _, w := range wants {
			if !x.due(w, time.Now()) {
				continue
			}
			if err := x.build(ctx, w); err != nil && ctx.Err() == nil {
				x.log().Warn("Live TV: build an XMLTV guide", "dvr", w.dvr, "error", err, "retryIn", cloudRetryAfter.String())
				x.failed(w.dvr, time.Now())
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-x.kick:
		case <-time.After(cloudFetchPoll):
		}
	}
}

// due reports whether w's guide wants building at now: never built by this
// holder (a build at once: the guide may be PMS's first fill, or a build
// another holder made of an older XMLTV); its hash moved and the last build
// is xmltvBuildFloor old; or the last build is xmltvBuildEvery old. A
// failed build waits cloudRetryAfter.
func (x *xmltvGuides) due(w xmltvWant, now time.Time) bool {
	x.mu.Lock()
	b, ok := x.built[strings.ToLower(w.dvr)]
	x.mu.Unlock()
	switch {
	case !ok:
		return true
	case !b.failed.IsZero() && now.Sub(b.failed) < cloudRetryAfter:
		return false
	case b.at.IsZero():
		return true
	case b.hash != w.hash && now.Sub(b.at) >= xmltvBuildFloor:
		return true
	}
	return now.Sub(b.at) >= xmltvBuildEvery
}

// failed records a failed build of dvr's guide at at: built again once
// cloudRetryAfter has passed, the guide it failed to replace keeping its
// age.
func (x *xmltvGuides) failed(dvr string, at time.Time) {
	k := strings.ToLower(dvr)
	x.mu.Lock()
	defer x.mu.Unlock()
	b := x.built[k]
	b.hash, b.failed = "", at
	x.built[k] = b
}

// build fetches w's lineup and XMLTV, caches them with the plan, and builds
// and installs the guide.
func (x *xmltvGuides) build(ctx context.Context, w xmltvWant) error {
	log := x.log().With("dvr", w.dvr)
	began := time.Now()
	lineup, err := fetchXMLTVLineup(ctx, w.url)
	if err != nil {
		return err
	}
	conn, err := x.m.connectGuides(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if err := epgcloud.EnsureCache(ctx, conn); err != nil {
		return err
	}
	fetched := time.Now()
	body, err := fetchXMLTV(ctx, w.url)
	if err != nil {
		return err
	}
	if err := epgcloud.CachePut(ctx, conn, xmltvKey(w.dvr, w.hash), body, fetched); err != nil {
		return err
	}
	p := xmltvPlan{Provider: guidebuild.ProviderXMLTV, DVR: w.dvr, URL: w.url, Hash: w.hash, Lineup: lineup}
	planBody, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := epgcloud.CachePut(ctx, conn, planPrefix+w.dvr, planBody, time.Now()); err != nil {
		return err
	}
	g := plexbootstrap.Guide{Provider: guidebuild.ProviderXMLTV, DVR: w.dvr}
	library := x.m.Config.Postgres.Schema
	var stats guidebuild.Stats
	inJob := x.m.Config.LiveTV.CloudGuideBuild != CloudGuideBuildInProcess
	if inJob {
		stamp := cloudPlanStamp(planBody)
		// The cloud guides' Job machinery, which reads nothing of a cloud
		// builder but the manager.
		jobs := &cloudGuides{m: x.m}
		if err := jobs.stageInJob(ctx, log, conn, g, stamp); err != nil {
			return err
		}
		if err := plexbootstrap.SwapStaged(ctx, log, conn, library, g, stamp); err != nil {
			return err
		}
	} else {
		in, err := xmltvInput(ctx, conn, p)
		if err != nil {
			return err
		}
		if err := plexbootstrap.InstallBuilt(ctx, log, conn, library, g, cloudBuildFunc(&in, &stats)); err != nil {
			return err
		}
	}
	now := time.Now()
	if err := plexbootstrap.MarkGuideRefreshed(ctx, conn, library, w.dvr, builtHorizon(ctx, conn, g, now), now); err != nil {
		log.Warn("Live TV: write a built guide's provider row", "error", err)
	}
	x.mu.Lock()
	x.built[strings.ToLower(w.dvr)] = cloudBuilt{hash: w.hash, at: now}
	x.mu.Unlock()
	// The DVR's older XMLTV bodies are read by no plan any more.
	if err := epgcloud.CachePrune(ctx, conn, xmltvPrefix+w.dvr+"/", fetched); err != nil {
		log.Warn("Live TV: prune the XMLTV guides' cache", "error", err)
	}
	took := time.Since(began).Round(time.Second).String()
	if inJob {
		log.Info("Live TV: built an XMLTV guide in a Job", "hash", w.hash, "took", took)
		return nil
	}
	log.Info("Live TV: built an XMLTV guide", "hash", w.hash, "channels", stats.Channels, "items", stats.Items,
		"airings", stats.Airings, "took", took)
	return nil
}

// builtHorizon is when g's guide ends, its latest airing's end, read from
// its schema; or else.
func builtHorizon(ctx context.Context, conn *pgx.Conn, g plexbootstrap.Guide, orElse time.Time) time.Time {
	var ends *int64
	err := conn.QueryRow(ctx, "SELECT max(ends_at) FROM "+pgx.Identifier{g.Schema(), "media_items"}.Sanitize()).Scan(&ends)
	if err != nil || ends == nil {
		return orElse
	}
	return time.Unix(*ends, 0)
}

// siblingURL is the file name beside u's last path segment: a device's
// lineup.json beside its xmltv.xml.
func siblingURL(u, name string) (string, error) {
	parsed, err := url.Parse(u)
	if err != nil {
		return "", err
	}
	parsed.Path = path.Join(path.Dir(parsed.Path), name)
	parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", ""
	return parsed.String(), nil
}

// tunerGet GETs u from the tuner, failing on any status but 200.
func tunerGet(ctx context.Context, u string, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// fetchXMLTVLineup reads the device's lineup.json beside the guide at
// guideURL into the XMLTV channel id of each of its numbers. clustarr's
// XMLTV names each channel by its number, so the map is the identity; it
// is kept a map so another id scheme needs no other code.
func fetchXMLTVLineup(ctx context.Context, guideURL string) (map[string]string, error) {
	u, err := siblingURL(guideURL, "lineup.json")
	if err != nil {
		return nil, fmt.Errorf("the device's lineup beside %s: %w", guideURL, err)
	}
	resp, err := tunerGet(ctx, u, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("read the device's lineup: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var entries []struct {
		GuideNumber string `json:"GuideNumber"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, xmltvMaxLineup)).Decode(&entries); err != nil {
		return nil, fmt.Errorf("read the device's lineup: %w", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if n := strings.TrimSpace(e.GuideNumber); n != "" {
			out[n] = n
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the device's lineup lists no channel")
	}
	return out, nil
}

// fetchXMLTV reads the guide at guideURL, at most xmltvMaxBody, gzipped for
// the cache unless it came gzipped (xmltvguide.Read takes either).
func fetchXMLTV(ctx context.Context, guideURL string) ([]byte, error) {
	resp, err := tunerGet(ctx, guideURL, xmltvFetchTimeout)
	if err != nil {
		return nil, fmt.Errorf("read the guide: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	src := bufio.NewReader(io.LimitReader(resp.Body, xmltvMaxBody+1))
	var buf bytes.Buffer
	var n int64
	if magic, _ := src.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		n, err = io.Copy(&buf, src)
	} else {
		gz, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
		if n, err = io.Copy(gz, src); err == nil {
			err = gz.Close()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("read the guide: %w", err)
	}
	if n > xmltvMaxBody {
		return nil, fmt.Errorf("the guide at %s is over %d MiB", guideURL, xmltvMaxBody>>20)
	}
	return buf.Bytes(), nil
}

// errEmptyXMLTV is an XMLTV with no programme to build: never built over a
// live guide.
var errEmptyXMLTV = errors.New("the XMLTV holds no programme from now on, on any channel of the device's lineup")

// xmltvInput reads a build's XMLTV from the cache into its input, refusing
// one with nothing to air.
func xmltvInput(ctx context.Context, conn *pgx.Conn, p xmltvPlan) (guidebuild.Input, error) {
	body, _, ok, err := epgcloud.CacheGet(ctx, conn, xmltvKey(p.DVR, p.Hash))
	if err != nil {
		return guidebuild.Input{}, err
	}
	if !ok {
		return guidebuild.Input{}, fmt.Errorf("the cache holds no XMLTV for DVR %s at %q", p.DVR, p.Hash)
	}
	return readXMLTV(body, p.Lineup, time.Now().UTC())
}

// readXMLTV reads an XMLTV body into a build's input at now, refusing one
// none of whose lineup's channels has an airing that has not ended by now
// (errEmptyXMLTV): the planner would leave every airing out, and the swap
// would replace a live guide with an empty one.
func readXMLTV(body []byte, lineup map[string]string, now time.Time) (guidebuild.Input, error) {
	in, _, err := xmltvguide.Read(bytes.NewReader(body), lineup, now)
	if err != nil {
		return in, fmt.Errorf("read the XMLTV: %w", err)
	}
	for _, grid := range in.Grids {
		for _, items := range grid {
			for _, it := range items {
				for _, a := range it.Media {
					if a.EndsAt > now.Unix() {
						return in, nil
					}
				}
			}
		}
	}
	return in, errEmptyXMLTV
}
