package main

import (
	"errors"
	"fmt"
	"net/netip"
	"path"
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
	// LocalAddress is the link-local address every Plex pod adds to lo and
	// serves the blocks' ports on. Default 169.254.47.1.
	LocalAddress string `mapstructure:"localAddress"`
	// RestartEvery bounds how often the lease holder restarts PMS on the
	// other pods after a DVR change. Default 10m.
	RestartEvery time.Duration `mapstructure:"restartEvery"`
	// StaleGrabAfter is how long a Live TV grab may go unwatched before
	// its pod ends it (pkg/plex/livetv.StaleGrabs). Default 5m.
	StaleGrabAfter time.Duration `mapstructure:"staleGrabAfter"`
}

// defaultRestartEvery is plex.liveTV.restartEvery's default.
const defaultRestartEvery = 10 * time.Minute

// metadataAddress is the cloud metadata service's link-local address,
// which a Live TV address must never shadow.
var metadataAddress = netip.MustParseAddr("169.254.169.254")

// loadLiveTV reads plex.liveTV; it needs the clustarr namespace it watches.
func loadLiveTV(v *viper.Viper, cl ClustarrConfig) (LiveTVConfig, error) {
	var lt LiveTVConfig
	if err := v.UnmarshalKey(liveTVKey, &lt, func(dc *mapstructure.DecoderConfig) { dc.WeaklyTypedInput = true }); err != nil {
		return lt, fmt.Errorf("read %s: %w", liveTVKey, err)
	}
	if lt.Language == "" {
		lt.Language = "eng"
	}
	if lt.LocalAddress == "" {
		lt.LocalAddress = plexlivetv.DefaultLocalAddress
	}
	addr, err := netip.ParseAddr(lt.LocalAddress)
	if err != nil || !addr.Is4() || !addr.IsLinkLocalUnicast() || addr == metadataAddress {
		return lt, fmt.Errorf("%s.localAddress: %q: must be an IPv4 link-local address (169.254.0.0/16) other than %s", liveTVKey, lt.LocalAddress, metadataAddress)
	}
	if lt.RestartEvery <= 0 {
		lt.RestartEvery = defaultRestartEvery
	}
	if lt.StaleGrabAfter <= 0 {
		lt.StaleGrabAfter = plexlivetv.DefaultStaleGrabAfter
	}
	if lt.Enabled && cl.Namespace == "" {
		return lt, fmt.Errorf("%s.enabled: needs %s.namespace, where clustarr's IPTVProviders live", liveTVKey, clustarrKey)
	}
	return lt, nil
}
