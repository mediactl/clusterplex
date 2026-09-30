package main

import (
	"errors"
	"fmt"
	"path"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
	plexprovision "github.com/mediactl/clusterplex/pkg/plex/provision"
)

const (
	providersKey = "plex.metadataProviders"
	librariesKey = "plex.libraries"
	clustarrKey  = "plex.clustarr"
)

// ClustarrConfig is the clustarr install whose changes Plex follows
// (spec §6). Off, nothing watches clustarr.
type ClustarrConfig struct {
	Enabled      bool                    `mapstructure:"enabled"`
	Namespace    string                  `mapstructure:"namespace"`
	PathMappings []clustarrwatch.Mapping `mapstructure:"pathMappings"`
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
	return prov, cl, errors.Join(errs...)
}
