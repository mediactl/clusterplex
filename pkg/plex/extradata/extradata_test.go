package extradata

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The provider row PMS wrote for the rig's cloud guide, 2026-11-10.
const pmsProviderRow = `{"pv:epgSource":"Gracenote","pv:lastChunkEndedAt":"1795478400","pv:timeOfLastRefresh":"1794280697",` +
	`"url":"pv%3AepgSource=Gracenote&pv%3AlastChunkEndedAt=1795478400&pv%3AtimeOfLastRefresh=1794280697"}`

func TestExtraDataIsWrittenAsPMSWritesIt(t *testing.T) {
	assert.Equal(t, pmsProviderRow, Encode(map[string]string{
		"pv:timeOfLastRefresh": "1794280697", "pv:epgSource": "Gracenote", "pv:lastChunkEndedAt": "1795478400",
	}))
	assert.Equal(t, "https%3A%2F%2Fprovider-static%2Eplex%2Etv%2Fa%2Fx_y-z%20%28HD%29", Escape("https://provider-static.plex.tv/a/x_y-z (HD)"))
}

func TestExtraDataReadsBackWithoutItsURL(t *testing.T) {
	f, err := Decode(pmsProviderRow)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"pv:epgSource": "Gracenote", "pv:lastChunkEndedAt": "1795478400", "pv:timeOfLastRefresh": "1794280697"}, f)
	f, err = Decode("")
	require.NoError(t, err)
	assert.Empty(t, f)
}
