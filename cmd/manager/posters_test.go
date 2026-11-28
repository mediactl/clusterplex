package main

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// plex.compactPosters.mode is off unless set: a chart that does not know
// the key runs nothing new. observe has the proxy count poster requests;
// serve is not built yet and watches as observe does rather than refuse to
// start, since a manager that does not start is a Plex that does not.
func TestCompactPostersModeIsOffUnlessSet(t *testing.T) {
	load := func(section map[string]any) (CompactPostersConfig, error) {
		v := viper.New()
		if section != nil {
			v.Set(compactPostersKey, section)
		}
		return loadCompactPosters(v)
	}
	for _, tc := range []struct {
		name     string
		section  map[string]any
		observes bool
	}{
		{"unset", nil, false},
		{"empty", map[string]any{"mode": ""}, false},
		{"off", map[string]any{"mode": "off"}, false},
		{"observe", map[string]any{"mode": "observe"}, true},
		{"serve watches until it is built", map[string]any{"mode": "serve"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp, err := load(tc.section)
			require.NoError(t, err)
			assert.Equal(t, tc.observes, cp.Observes())
		})
	}
	_, err := load(map[string]any{"mode": "on"})
	assert.Error(t, err)
}
