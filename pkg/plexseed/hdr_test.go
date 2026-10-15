package plexseed_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plexseed"
)

// An HDR probe lands in media_items.color_trc, and every row carries Plex's
// analysis version so Plex never analyses a file clustarr already probed.
func TestAnHDRProbeSetsColorTRCAndTheAnalysisVersion(t *testing.T) {
	row, _ := plexseed.MediaRows(plexseed.Probe{Container: "mkv", VideoCodec: "hevc", HDR: "hdr10", Width: 3840, Height: 2160}, "h", "en")
	require.Equal(t, "smpte2084", row.ColorTRC)
	require.Equal(t, plexseed.MediaAnalysisVersion, row.AnalysisVersion)
	sdr, _ := plexseed.MediaRows(plexseed.Probe{Container: "mkv", VideoCodec: "h264"}, "h", "en")
	require.Equal(t, "bt709", sdr.ColorTRC)
}

// clustarr's hdr enum: none and "" are SDR, HLG is its own curve, and every
// PQ flavour (HDR10, HDR10+, PQ10, Dolby Vision) is smpte2084.
func TestHLGAndDolbyVisionMapToTheirTransferCurves(t *testing.T) {
	for in, want := range map[string]string{"hlg10": "arib-std-b67", "dolbyVisionHlg": "arib-std-b67", "dolbyVision": "smpte2084", "dolbyVisionHdr10": "smpte2084", "dolbyVisionHdr10Plus": "smpte2084", "hdr10plus": "smpte2084", "pq10": "smpte2084", "dolbyVisionSdr": "bt709", "none": "bt709", "": "bt709"} {
		row, _ := plexseed.MediaRows(plexseed.Probe{HDR: in}, "h", "en")
		require.Equal(t, want, row.ColorTRC, in)
	}
}

// A commentary track and a bitmap subtitle are flagged in their stream's
// extra_data, where Plex's own analysis records the same facts.
func TestCommentaryAndBitmapFlagsReachTheStreamExtra(t *testing.T) {
	_, streams := plexseed.MediaRows(plexseed.Probe{
		VideoCodec: "h264",
		Audio:      []plexseed.Audio{{Index: 1, Codec: "aac", Commentary: true}},
		Subtitles:  []plexseed.Subtitle{{Index: 2, Codec: "pgs", Bitmap: true}},
	}, "h", "en")
	require.Len(t, streams, 3)
	require.Contains(t, streams[1].Extra, `"ma:commentary":"1"`)
	require.Contains(t, streams[2].Extra, `"ma:bitmap":"1"`)
}

// SeedTx seeds one known target inside the caller's transaction, writing
// the analysis version and the transfer curve with the codec columns.
func TestSeedTxWritesTheAnalysisVersionSoPlexNeverAnalyses(t *testing.T) {
	conn := plexDB(t)
	l := insertFile(t, conn, "Clustarr Movies")
	s := seeder(conn)
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	_, err = s.SeedTx(ctx, tx, plexseed.Target{Part: l.part, Media: l.media, Item: l.item},
		plexseed.Input{Path: l.path, ProbeHash: "h1", Probe: &plexseed.Probe{Container: "mkv", VideoCodec: "hevc", HDR: "hdr10", RuntimeMillis: 60000}})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	require.Equal(t, int(plexseed.MediaAnalysisVersion), count(t, conn, "SELECT media_analysis_version FROM media_items WHERE id = $1", l.media))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND color_trc = 'smpte2084'", l.media))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1", l.media), "the video stream")
}
