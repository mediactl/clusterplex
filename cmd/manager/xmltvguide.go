package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/xml"
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
	// xmltvMaxBody caps a block's XMLTV as fetched, and xmltvMaxLineup its
	// lineup.json. A category's XMLTV is 14 days of Gracenote listings,
	// about 0.5 MB a paired channel, so a block of 400 paired channels is
	// about 200 MB (a week of iptv-epg.org's 450 channels was about 20 MB,
	// and the 64 MiB this was sized for it refused about 130 paired
	// channels). The build Job's limit is 2 GiB, and xmltvguide.Read holds
	// about 4x its body.
	xmltvMaxBody   = 256 << 20
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
	// known is the schemas of the guides a build installed, by this holder
	// or one before it (learnBuilt, at each PMS start): the managers' from
	// the start, before the converger names them, so no load PMS left in a
	// guide's file is copied over one.
	known map[string]bool
	kick  chan struct{}
	// lastBuild reads when dvr's guide was last built and from which hash
	// (storedBuild); a variable for the tests.
	lastBuild func(ctx context.Context, dvr string) (cloudBuilt, error)
}

func newXMLTVGuides(m *Manager) *xmltvGuides {
	x := &xmltvGuides{
		m: m, wants: map[string]xmltvWant{}, built: map[string]cloudBuilt{}, known: map[string]bool{},
		kick: make(chan struct{}, 1),
	}
	x.lastBuild = x.storedBuild
	return x
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

// ours reports whether g's guide is the managers': an XMLTV DVR's the
// converger has named (wanted), or one a build installed (known). Nil has
// none.
func (x *xmltvGuides) ours(g plexbootstrap.Guide) bool {
	if x == nil || g.Provider != guidebuild.ProviderXMLTV {
		return false
	}
	if x.wanted(g.DVR) {
		return true
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.known[g.Schema()]
}

// learnBuilt adds every guide a build installed (plexbootstrap's
// BuiltGuideSchemas) to known.
func (x *xmltvGuides) learnBuilt(ctx context.Context, conn *pgx.Conn) error {
	schemas, err := plexbootstrap.BuiltGuideSchemas(ctx, conn)
	if err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, s := range schemas {
		x.known[s] = true
	}
	return nil
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
			if !x.due(ctx, w, time.Now()) {
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

// due reports whether w's guide wants building at now: never built (a
// build at once: the guide is PMS's first fill); its hash moved and the
// last build is xmltvBuildFloor old; or the last build is xmltvBuildEvery
// old. A failed build waits cloudRetryAfter. At first sight a holder reads
// the last build from the guide and its plan (lastBuild), so a restart
// builds again only what moved or aged.
func (x *xmltvGuides) due(ctx context.Context, w xmltvWant, now time.Time) bool {
	k := strings.ToLower(w.dvr)
	x.mu.Lock()
	b, ok := x.built[k]
	x.mu.Unlock()
	if !ok {
		last, err := x.lastBuild(ctx, w.dvr)
		if err != nil {
			x.log().Warn("Live TV: read when an XMLTV guide was last built", "dvr", w.dvr, "error", err)
			return false
		}
		if last.at.IsZero() {
			return true
		}
		b = last
		x.mu.Lock()
		x.built[k] = b
		x.mu.Unlock()
	}
	switch {
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

// storedBuild is when dvr's guide was last built and the hash it was built
// from, as the guide's schema and the plan cached for it say: zero when it
// never was, a load has replaced it since, or a build is under way or staged
// (built again, which adopts that build by its stamp, as cloud guides do);
// the hash empty, so a moved one is assumed, when the cached plan is no
// XMLTV plan or was written after the build (one that failed).
func (x *xmltvGuides) storedBuild(ctx context.Context, dvr string) (cloudBuilt, error) {
	conn, err := x.m.connectGuides(ctx)
	if err != nil {
		return cloudBuilt{}, err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	at, err := plexbootstrap.GuideBuiltAt(ctx, conn, plexbootstrap.Guide{Provider: guidebuild.ProviderXMLTV, DVR: dvr})
	if err != nil || at.IsZero() {
		return cloudBuilt{}, err
	}
	if (&cloudGuides{m: x.m, logger: x.log()}).inFlight(ctx, dvr) {
		return cloudBuilt{}, nil
	}
	if err := epgcloud.EnsureCache(ctx, conn); err != nil {
		return cloudBuilt{}, err
	}
	b := cloudBuilt{at: at}
	body, planned, ok, err := cachedPlan(ctx, conn, dvr)
	if err != nil {
		return cloudBuilt{}, err
	}
	var p xmltvPlan
	if ok && !planned.After(at) && planProvider(body) == guidebuild.ProviderXMLTV && json.Unmarshal(body, &p) == nil {
		b.hash = p.Hash
	}
	return b, nil
}

// build fetches w's lineup and XMLTV, caches them with the plan, and builds
// and installs the guide. An XMLTV with nothing to air is refused here,
// before anything is cached or a Job started (the Job refuses it too).
func (x *xmltvGuides) build(ctx context.Context, w xmltvWant) error {
	log := x.log().With("dvr", w.dvr)
	began := time.Now()
	lineup, err := fetchXMLTVLineup(ctx, w.url)
	if err != nil {
		return err
	}
	fetched := time.Now()
	body, err := fetchXMLTV(ctx, w.url)
	if err != nil {
		return err
	}
	if airs, err := xmltvAirs(body, lineup, time.Now()); err != nil {
		return fmt.Errorf("read the XMLTV: %w", err)
	} else if !airs {
		return errEmptyXMLTV
	}
	conn, err := x.m.connectGuides(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if err := epgcloud.EnsureCache(ctx, conn); err != nil {
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
		// builder but the manager, logging as this builder.
		jobs := &cloudGuides{m: x.m, logger: x.log()}
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
	x.known[g.Schema()] = true
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

// xmltvAirs reports whether an XMLTV body (gzip or plain) holds a programme
// on a channel lineup names whose stop is past now: the holder's look
// before it plans a build, which reads only as far as the first such
// programme and decodes nothing else, where the build's reading
// (readXMLTV) decodes the whole guide.
func xmltvAirs(body []byte, lineup map[string]string, now time.Time) (bool, error) {
	var r io.Reader = bytes.NewReader(body)
	if len(body) > 1 && body[0] == 0x1f && body[1] == 0x8b {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return false, err
		}
		r = gz
	}
	d := xml.NewDecoder(r)
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "programme" {
			continue
		}
		var channel, stop string
		for _, a := range se.Attr {
			switch a.Name.Local {
			case "channel":
				channel = strings.TrimSpace(a.Value)
			case "stop":
				stop = strings.TrimSpace(a.Value)
			}
		}
		if lineup[channel] == "" {
			continue
		}
		if end, ok := xmltvguide.ParseTime(stop); ok && end.After(now) {
			return true, nil
		}
	}
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
