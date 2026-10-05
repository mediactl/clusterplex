package remux

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const arcane = `FFMPEG_EXTERNAL_LIBS='/c/' X_PLEX_TOKEN=tok "/usr/lib/plexmediaserver/Plex Transcoder" -codec:0 hevc -codec:1 aac -ss 495 -noaccurate_seek -analyzeduration 20000000 -probesize 20000000 -i "/library/tv/Arcane (2021) {tvdb-371028}/Season 01/Arcane - S01E07.mkv" -start_at_zero -copyts -fps_mode cfr -y -nostats -loglevel quiet -loglevel_plex error -progressurl http://127.0.0.1:32400/video/:/transcode/session/f168tc7vl2desp78pov5ktwu/b12394b4-a41e-475d-bf98-4a9e52ad87c5/progress -map 0:0 -codec:0 copy -filter_complex "[0:1] aresample=async=1:ochl='stereo':rematrix_maxval=0.000000dB:osr=96000[0]" -map "[0]" -metadata:s:1 language=eng -codec:1 aac -b:1 256k -f dash -seg_duration 5 -dash_segment_type mp4 -init_seg_name 'init-stream$RepresentationID$.m4s' -media_seg_name 'chunk-stream$RepresentationID$-$Number%05d$.m4s' -window_size 5 -delete_removed false -skip_to_segment 100 -manifest_name "http://127.0.0.1:32400/video/:/transcode/session/f168tc7vl2desp78pov5ktwu/b12394b4-a41e-475d-bf98-4a9e52ad87c5/manifest?X-Plex-Http-Pipeline=infinite" -avoid_negative_ts disabled -map_metadata -1 -map_chapters -1 dash`

func TestParseReadsABrowserRemux(t *testing.T) {
	args, env := loggedJob(arcane)
	got, err := Parse(args, env)
	require.NoError(t, err)
	assert.Equal(t, Job{
		Input:           "/library/tv/Arcane (2021) {tvdb-371028}/Season 01/Arcane - S01E07.mkv",
		Start:           495 * time.Second,
		SkipToSegment:   100,
		SegmentDuration: 5 * time.Second,
		VideoStream:     0,
		AudioStream:     1,
		AudioChannels:   2,
		AudioSampleRate: 96000,
		AudioBitRate:    256000,
		AudioLanguage:   "eng",
		ProgressURL:     "http://127.0.0.1:32400/video/:/transcode/session/f168tc7vl2desp78pov5ktwu/b12394b4-a41e-475d-bf98-4a9e52ad87c5/progress",
		ManifestURL:     "http://127.0.0.1:32400/video/:/transcode/session/f168tc7vl2desp78pov5ktwu/b12394b4-a41e-475d-bf98-4a9e52ad87c5/manifest?X-Plex-Http-Pipeline=infinite",
		Token:           "tok",
	}, got)
	assert.Equal(t, "f168tc7vl2desp78pov5ktwu", got.SessionID())
}

// Every DASH job plex-0 ran on 2026-10-01 is one this worker can take,
// except the 5 that also stream subtitles (a second, ASS segment output):
// subtitles stay Plex's.
func TestEveryLoggedBrowserJobParses(t *testing.T) {
	refused := 0
	for i, args := range loggedJobs(t) {
		j, err := Parse(args, nil)
		if slices.Contains(args, "-segment_format") {
			assert.ErrorIs(t, err, ErrNotRemux, "job %d streams subtitles", i)
			refused++
			continue
		}
		require.NoError(t, err, "job %d", i)
		assert.Equal(t, 5*time.Second, j.SegmentDuration, "job %d", i)
		assert.GreaterOrEqual(t, j.SkipToSegment, 1, "job %d", i)
		assert.NotEmpty(t, j.Input, "job %d", i)
	}
	assert.Equal(t, 5, refused)
}

func TestAnUnknownOptionIsRefused(t *testing.T) {
	base, env := loggedJob(arcane)
	for name, mutate := range map[string]func([]string) []string{
		"a video encode": func(a []string) []string {
			i := slices.Index(a, "copy")
			a[i] = "libx264"
			return a
		},
		"HLS output": func(a []string) []string {
			i := slices.Index(a, "dash")
			a[i] = "segment"
			return a
		},
		"a subtitle burn-in": func(a []string) []string {
			i := slices.Index(a, "-filter_complex")
			a[i+1] = "[0:0][0:3]overlay[0]"
			return a
		},
		"an option this parser does not know": func(a []string) []string {
			i := slices.Index(a, "-y")
			return slices.Insert(a, i, "-ac", "6")
		},
		"a third output": func(a []string) []string {
			i := slices.Index(a, "-f")
			return slices.Insert(a, i, "-map", "0:2", "-codec:2", "webvtt")
		},
		"other segment names": func(a []string) []string {
			i := slices.Index(a, "-media_seg_name")
			a[i+1] = "seg-$Number$.m4s"
			return a
		},
	} {
		_, err := Parse(mutate(slices.Clone(base)), env)
		assert.True(t, errors.Is(err, ErrNotRemux), "%s: %v", name, err)
	}
}

func TestACopiedAudioTrackIsARemux(t *testing.T) {
	args, env := loggedJob(arcane)
	i := slices.Index(args, "-filter_complex")
	args = slices.Delete(args, i, i+2)
	m := slices.Index(args, `[0]`)
	args[m] = "0:1"
	c := slices.Index(args, "-i") + slices.Index(args[slices.Index(args, "-i"):], "-codec:1") // the output's, not the decoder hint before -i
	args[c+1] = "copy"
	b := slices.Index(args, "-b:1")
	args = slices.Delete(args, b, b+2)
	got, err := Parse(args, env)
	require.NoError(t, err)
	assert.True(t, got.AudioCopy)
	assert.Equal(t, 1, got.AudioStream)
	assert.Zero(t, got.AudioBitRate)
}
