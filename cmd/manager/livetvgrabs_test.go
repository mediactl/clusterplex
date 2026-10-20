package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnUnwatchedLiveTVGrabIsEndedAfterFiveMinutesByDefault(t *testing.T) {
	podIdentity(t)
	c, err := loadConfig(nil)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, c.LiveTV.StaleGrabAfter)

	podIdentity(t)
	c, err = loadConfig([]string{"--config", writeConfig(t, "plex:\n  liveTV:\n    staleGrabAfter: 90s\n")})
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, c.LiveTV.StaleGrabAfter)
}
