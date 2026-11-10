package main

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
	plexlivetv "github.com/mediactl/clusterplex/pkg/plex/livetv"
	plexprovision "github.com/mediactl/clusterplex/pkg/plex/provision"
)

const (
	providersKey = "plex.metadataProviders"
	librariesKey = "plex.libraries"
	clustarrKey  = "plex.clustarr"
	liveTVKey    = "plex.liveTV"
)

// ClustarrConfig is the clustarr install whose changes Plex follows
// (spec §6). Off, nothing watches clustarr.
type ClustarrConfig struct {
	Enabled      bool                    `mapstructure:"enabled"`
	Namespace    string                  `mapstructure:"namespace"`
	PathMappings []clustarrwatch.Mapping `mapstructure:"pathMappings"`
	// TokenSecret names a Secret in Namespace holding the server's plex.tv
	// account: its token under "token" -- what clustarr's Plex watchlist
	// ImportList reads -- and its username and email, and the claim code
	// last spent. The first lease holder writes it from Preferences.xml when
	// it has no token; from then on it is the master copy every replica is
	// held to (servertoken). Empty keeps none.
	TokenSecret string `mapstructure:"tokenSecret"`
}

// loadProvisioning reads what the lease holder provisions into Plex and the
// clustarr install it follows. Both are lists rather than maps, because
// viper lowercases map keys and Plex's are case sensitive.
func loadProvisioning(v *viper.Viper) (plexprovision.Config, ClustarrConfig, error) {
	var (
		prov plexprovision.Config
		cl   ClustarrConfig
		errs []error
	)
	weak := func(dc *mapstructure.DecoderConfig) { dc.WeaklyTypedInput = true }
	if err := v.UnmarshalKey(providersKey, &prov.Providers, weak); err != nil {
		errs = append(errs, fmt.Errorf("read %s: %w", providersKey, err))
	}
	if err := v.UnmarshalKey(librariesKey, &prov.Libraries, weak); err != nil {
		errs = append(errs, fmt.Errorf("read %s: %w", librariesKey, err))
	}
	if err := v.UnmarshalKey(clustarrKey, &cl, weak); err != nil {
		errs = append(errs, fmt.Errorf("read %s: %w", clustarrKey, err))
	}
	if err := prov.Validate(); err != nil {
		errs = append(errs, err)
	}
	if cl.Enabled {
		if cl.Namespace == "" {
			errs = append(errs, fmt.Errorf("%s.namespace: required when enabled", clustarrKey))
		}
		if len(cl.PathMappings) == 0 {
			errs = append(errs, fmt.Errorf("%s.pathMappings: at least one is required when enabled", clustarrKey))
		}
		seen := map[string]bool{}
		for _, m := range cl.PathMappings {
			if !path.IsAbs(m.Clustarr) || !path.IsAbs(m.Plex) {
				errs = append(errs, fmt.Errorf("%s.pathMappings: %q -> %q: both sides must be absolute", clustarrKey, m.Clustarr, m.Plex))
			}
			from := path.Clean(m.Clustarr)
			if seen[from] {
				errs = append(errs, fmt.Errorf("%s.pathMappings: %q is mapped twice", clustarrKey, m.Clustarr))
			}
			seen[from] = true
		}
	}
	if cl.TokenSecret != "" {
		if !cl.Enabled {
			errs = append(errs, fmt.Errorf("%s.tokenSecret: needs %s.enabled", clustarrKey, clustarrKey))
		}
		for _, msg := range validation.IsDNS1123Subdomain(cl.TokenSecret) {
			errs = append(errs, fmt.Errorf("%s.tokenSecret: %q: %s", clustarrKey, cl.TokenSecret, msg))
		}
	}
	return prov, cl, errors.Join(errs...)
}

// LiveTVConfig adds clustarr's IPTVProviders to Plex as DVRs (clustarr's
// Live TV spec 2026-10-07 §6.1), watching plex.clustarr.namespace. Off, no
// DVR is touched.
type LiveTVConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Language is the DVRs' guide language. Default "eng".
	Language string `mapstructure:"language"`
	// RestartEvery bounds how often the lease holder restarts PMS on the
	// other pods after a DVR change. Default 10m.
	RestartEvery time.Duration `mapstructure:"restartEvery"`
	// StaleGrabAfter is how long a Live TV grab may go unwatched before
	// its pod ends it (pkg/plex/livetv.StaleGrabs). Default 5m.
	StaleGrabAfter time.Duration `mapstructure:"staleGrabAfter"`
	// GuideRefreshWindow is the UTC hours, "start-end", within which a
	// Plex cloud lineup's guide is reloaded once a day
	// (pkg/plex/livetv.Converger.CloudRefresh); "none" turns it off.
	// Default "8-11": 02:00-05:00 US Central standard time.
	GuideRefreshWindow string `mapstructure:"guideRefreshWindow"`
	// GuideLoadsInPostgres has the shim stage every guide load PMS makes
	// in PostgreSQL (PLEX_PG_GUIDE_LOADS), where the load is otherwise
	// made in a SQLite file and copied at PMS's rename. Default false.
	GuideLoadsInPostgres bool `mapstructure:"guideLoadsInPostgres"`
	// SeveralPods allows Live TV on more than one Plex pod. The guides then
	// have one writer, the plex.tv Lease holder's PMS (guidewriter.go), but
	// every PMS still records every recording rule: no pod owns a
	// recording yet. Default false (liveTVNeedsOnePod).
	SeveralPods bool `mapstructure:"severalPods"`
	// CloudGuide is who fills a Plex cloud lineup's guide: "pms" (PMS
	// loads and refreshes it) or "manager" (the managers build it from
	// Plex's guide service, and PMS's own writes to it are held). Default
	// "pms". docs/superpowers/specs/2026-11-10-guide-writer-and-built-cloud-guide-design.md.
	CloudGuide string `mapstructure:"cloudGuide"`
}

// Who fills a Plex cloud lineup's guide (LiveTVConfig.CloudGuide).
const (
	CloudGuidePMS     = "pms"
	CloudGuideManager = "manager"
)

// GuideWriter reports whether the guides have one writer: the shim holds
// guide writes the manager's writer file does not allow. It is on when
// several pods may run Live TV, or when the managers build the cloud
// guides, whose PMS writes every pod holds. With one pod and PMS's own
// cloud guides it is off, and the shim writes as it always did.
func (lt LiveTVConfig) GuideWriter() bool {
	return lt.Enabled && (lt.SeveralPods || lt.CloudGuide == CloudGuideManager)
}

// defaultGuideRefreshWindow is plex.liveTV.guideRefreshWindow's default.
const defaultGuideRefreshWindow = "8-11"

// parseWindow reads an "HH-HH" window of UTC hours, "none" or "" as the
// empty window, into the hours [start, end), each 0-23.
func parseWindow(s string) ([2]int, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "none") || strings.EqualFold(s, "off") {
		return [2]int{}, nil
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return [2]int{}, fmt.Errorf("%q: want \"start-end\" in UTC hours, e.g. 8-11", s)
	}
	var w [2]int
	for i, part := range []string{a, b} {
		h, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || h < 0 || h > 23 {
			return [2]int{}, fmt.Errorf("%q: hour %q is not 0-23", s, part)
		}
		w[i] = h
	}
	if w[0] == w[1] {
		return [2]int{}, fmt.Errorf("%q: an empty window; use \"none\" to turn the refresh off", s)
	}
	return w, nil
}

// defaultRestartEvery is plex.liveTV.restartEvery's default.
const defaultRestartEvery = 10 * time.Minute

// loadLiveTV reads plex.liveTV; it needs the clustarr namespace it watches.
func loadLiveTV(v *viper.Viper, cl ClustarrConfig) (LiveTVConfig, error) {
	var lt LiveTVConfig
	if err := v.UnmarshalKey(liveTVKey, &lt, func(dc *mapstructure.DecoderConfig) { dc.WeaklyTypedInput = true }); err != nil {
		return lt, fmt.Errorf("read %s: %w", liveTVKey, err)
	}
	if lt.Language == "" {
		lt.Language = "eng"
	}
	if lt.RestartEvery <= 0 {
		lt.RestartEvery = defaultRestartEvery
	}
	if lt.StaleGrabAfter <= 0 {
		lt.StaleGrabAfter = plexlivetv.DefaultStaleGrabAfter
	}
	if lt.GuideRefreshWindow == "" {
		lt.GuideRefreshWindow = defaultGuideRefreshWindow
	}
	switch lt.CloudGuide {
	case "":
		lt.CloudGuide = CloudGuidePMS
	case CloudGuidePMS, CloudGuideManager:
	default:
		return lt, fmt.Errorf("%s.cloudGuide: %q is neither %q nor %q", liveTVKey, lt.CloudGuide, CloudGuidePMS, CloudGuideManager)
	}
	if _, err := parseWindow(lt.GuideRefreshWindow); err != nil {
		return lt, fmt.Errorf("%s.guideRefreshWindow: %w", liveTVKey, err)
	}
	if lt.Enabled && cl.Namespace == "" {
		return lt, fmt.Errorf("%s.enabled: needs %s.namespace, where clustarr's IPTVProviders live", liveTVKey, clustarrKey)
	}
	return lt, nil
}

// liveTVNeedsOnePod refuses Live TV on any Plex pod but a StatefulSet's
// first. Every PMS runs Live TV's background work on its own, and PMS has
// no setting to turn it off:
//   - it refreshes each DVR's guide at its start and on its own timer,
//     against the one guide the pods share in PostgreSQL, and a refresh is
//     not atomic, so two at once leave holes or doubled airings;
//   - it schedules and records the recording rules the pods share, keeping
//     its grabs in memory (the library has no table of them), so a second
//     PMS records every programme again.
//
// Found on the kind rig, 2026-11-09: a second pod's PMS began an
// incremental refresh of the cloud guide at its boot, beside the first,
// and started a recording scheduler of its own. So the deployment holds
// one Plex pod while Live TV is on. The chart refuses plex.replicas above 1
// with it; this is the same rule for an install the chart did not render
// (a kustomize base, a `kubectl scale`), checked before PMS starts, so the
// second PMS never runs.
//
// plex.liveTV.severalPods lifts it, for a deployment that records nothing:
// the guides then have one writer (guidewriter.go), but recordings do not.
func liveTVNeedsOnePod(podName string, lt LiveTVConfig) error {
	if !lt.Enabled || lt.SeveralPods {
		return nil
	}
	if n, ok := statefulSetOrdinal(podName); !ok || n == 0 {
		return nil
	}
	return fmt.Errorf("%s.enabled needs one Plex pod, and this is %s: every PMS refreshes Live TV's guides and "+
		"records its recording rules on its own, so a second pod refreshes the one shared guide beside the first "+
		"and records every programme again; run one Plex pod with Live TV, or set %s.severalPods if nothing "+
		"is recorded (docs/configuration.md, \"Live TV runs on one Plex pod\")", liveTVKey, podName, liveTVKey)
}

// statefulSetOrdinal is the ordinal a StatefulSet pod's name ends in; a
// name without one (a manager run outside a StatefulSet) has none.
func statefulSetOrdinal(name string) (int, bool) {
	i := strings.LastIndexByte(name, '-')
	if i < 0 || i == len(name)-1 {
		return 0, false
	}
	n, err := strconv.Atoi(name[i+1:])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
