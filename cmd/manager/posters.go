package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-viper/mapstructure/v2"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/viper"

	"github.com/mediactl/clusterplex/pkg/logging"
	"github.com/mediactl/clusterplex/pkg/plexposters"
	"github.com/mediactl/clusterplex/pkg/proxy"
)

const compactPostersKey = "plex.compactPosters"

// What plex.compactPosters.mode may be.
const (
	compactPostersOff     = "off"
	compactPostersObserve = "observe"
	compactPostersServe   = "serve"
)

// CompactPostersConfig is the proxy's part in clustarr's compact posters
// (clustarr's design of 2026-11-27, §5.4): a small poster a client asks
// Plex for is to be answered with the rendition whose badges and title
// image are drawn larger.
type CompactPostersConfig struct {
	// Mode is "off" (the default: poster requests are not looked at),
	// "observe" (every request still goes to Plex; the proxy counts what
	// clients ask for, clusterplex_poster_requests_total, and logs one line
	// per distinct kind of request) or "serve". Serving is not built: until
	// it is, "serve" observes.
	Mode string `mapstructure:"mode"`
}

// Observes reports whether the proxy looks at poster requests.
func (c CompactPostersConfig) Observes() bool {
	return c.Mode == compactPostersObserve || c.Mode == compactPostersServe
}

func loadCompactPosters(v *viper.Viper) (CompactPostersConfig, error) {
	var cp CompactPostersConfig
	if err := v.UnmarshalKey(compactPostersKey, &cp, func(dc *mapstructure.DecoderConfig) { dc.WeaklyTypedInput = true }); err != nil {
		return cp, fmt.Errorf("read %s: %w", compactPostersKey, err)
	}
	switch cp.Mode {
	case "":
		cp.Mode = compactPostersOff
	case compactPostersOff, compactPostersObserve, compactPostersServe:
	default:
		return cp, fmt.Errorf("%s.mode: %q is not off, observe or serve", compactPostersKey, cp.Mode)
	}
	return cp, nil
}

// thumbQuery is a row's poster as Plex holds it.
const thumbQuery = `SELECT COALESCE(user_thumb_url, '') FROM metadata_items WHERE id = $1`

// postersDiverter is the proxy's Divert watching the posters clients ask
// for; none when plex.compactPosters.mode is off. It claims no request. Its
// two loops, reading the rows asked about and following clustarr's pairing
// list, run until ctx ends.
func (m *Manager) postersDiverter(ctx context.Context) proxy.Divert {
	cp := m.Config.CompactPosters
	if !cp.Observes() {
		return nil
	}
	log := logging.SubsystemLogger(m.Logger, "posters")
	if cp.Mode == compactPostersServe {
		log.Warn("plex.compactPosters.mode is serve, which is not built: poster requests are observed and all go to Plex")
	}
	o := &plexposters.Observer{
		Thumb: func(ctx context.Context, id int64) (string, error) {
			var thumb string
			err := m.pool.QueryRow(ctx, thumbQuery, id).Scan(&thumb)
			if errors.Is(err, pgx.ErrNoRows) {
				return "", nil
			}
			return thumb, err
		},
		Metrics: plexposters.NewMetrics(prometheus.DefaultRegisterer),
		Logger:  log,
	}
	if ui := m.Config.Clustarr.UIURL; ui != "" {
		pairs := &plexposters.Pairs{URL: ui, Logger: log}
		o.Paired = pairs.Has
		go func() { _ = pairs.Run(ctx) }()
	}
	go func() { _ = o.Run(ctx) }()
	log.Info("poster requests are observed", "mode", cp.Mode)
	return o.Divert
}
