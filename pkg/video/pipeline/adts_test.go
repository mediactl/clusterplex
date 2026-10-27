package pipeline

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/video"
)

func TestAnADTSHeaderNamesItsAudioSpecificConfig(t *testing.T) {
	// Live TV's AAC arrives as ADTS in MPEG-TS; an fMP4's init needs the
	// AudioSpecificConfig aac_adtstoasc would build.
	for name, c := range map[string]struct {
		header []byte
		want   []byte
	}{
		// byte 2: profile (object type - 1) << 6 | frequency index << 2 | channels >> 2
		"LC 48 kHz stereo":   {[]byte{0xFF, 0xF1, 0x4C, 0x80, 0x2E, 0x7F, 0xFC}, []byte{0x11, 0x90}},
		"LC 44.1 kHz stereo": {[]byte{0xFF, 0xF1, 0x50, 0x80, 0x2E, 0x7F, 0xFC}, []byte{0x12, 0x10}},
		"LC 48 kHz 5.1":      {[]byte{0xFF, 0xF1, 0x4D, 0x80, 0x2E, 0x7F, 0xFC}, []byte{0x11, 0xB0}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := adtsConfig(c.header)
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}
	_, err := adtsConfig([]byte{0x47, 0x40, 0x11, 0x10, 0, 0, 0})
	require.ErrorIs(t, err, video.ErrShape, "no ADTS sync")
	_, err = adtsConfig([]byte{0xFF, 0xF1, 0x4C, 0x00, 0, 0, 0})
	require.ErrorIs(t, err, video.ErrShape, "channels in a program config element")
}
