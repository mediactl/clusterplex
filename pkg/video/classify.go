package video

import "strings"

// notVideo are the encoders that make no video: Plex's audio conversions
// and subtitle outputs. A remux copies its video; a conversion encodes it.
var notVideo = map[string]bool{
	"copy": true, "aac": true, "ac3": true, "eac3": true, "mp3": true, "libmp3lame": true,
	"libopus": true, "opus": true, "flac": true, "alac": true,
	"srt": true, "subrip": true, "ass": true, "ssa": true, "mov_text": true, "webvtt": true,
}

// IsConversion reports whether a Plex Transcoder argv converts video: some
// output stream's codec is neither a copy nor an audio or subtitle encoder.
// It reads the argv's structure alone. An encoder it does not know counts
// as video, so such a job is refused by the pool rather than run on Plex's
// node.
func IsConversion(args []string) bool {
	last := lastInput(args)
	if last < 0 {
		return false
	}
	for i := last + 2; i+1 < len(args); i++ {
		base, _, _ := strings.Cut(args[i], ":")
		if base != "-codec" && base != "-c" && base != "-vcodec" {
			continue
		}
		v := args[i+1]
		if !notVideo[v] && !strings.HasPrefix(v, "pcm_") {
			return true
		}
		i++
	}
	return false
}
