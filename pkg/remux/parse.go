package remux

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// downmix is the only audio filter Plex Web's jobs carry: input stream N
// resampled to stereo at osr, output label [0].
var downmix = regexp.MustCompile(`^\[0:(\d+)\] aresample=async=1:ochl='stereo':rematrix_maxval=0\.000000dB:osr=(\d+)\[0\]$`)

// streamRef reads "0:N", the only input Plex maps from.
var streamRef = regexp.MustCompile(`^0:(\d+)$`)

// valueOptions take one argument this parser reads or knowingly ignores;
// flagOptions take none. Anything else is refused, so a Plex release that
// adds an option runs on Plex's transcoder rather than on a guess.
var valueOptions = map[string]bool{
	"-codec:0": true, "-codec:1": true, "-analyzeduration": true, "-probesize": true,
	"-ss": true, "-i": true, "-fps_mode": true, "-loglevel": true, "-loglevel_plex": true,
	"-progressurl": true, "-map": true, "-filter_complex": true, "-metadata:s:1": true,
	"-b:1": true, "-f": true, "-seg_duration": true, "-dash_segment_type": true,
	"-init_seg_name": true, "-media_seg_name": true, "-window_size": true,
	"-delete_removed": true, "-skip_to_segment": true, "-manifest_name": true,
	"-avoid_negative_ts": true, "-map_metadata": true, "-map_chapters": true,
	// Plex's EasyAudioEncoder handoff for an EAC3 source (32 of the 44 real
	// jobs); ffgo decodes EAC3 itself, so it is read and ignored.
	"-eae_prefix:1": true,
}

var flagOptions = map[string]bool{
	"-noaccurate_seek": true, "-start_at_zero": true, "-copyts": true, "-y": true, "-nostats": true,
}

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrNotRemux, fmt.Sprintf(format, a...))
}

// Parse reads a Plex Transcoder argv. It accepts exactly a DASH stream of
// two outputs: output 0 a copied input stream, output 1 an input stream
// copied or converted to AAC (through Plex's stereo downmix or not).
func Parse(args []string, env map[string]string) (Job, error) {
	j := Job{Token: env["X_PLEX_TOKEN"]}
	if len(args) == 0 || args[len(args)-1] != "dash" {
		return j, refuse("the output is not dash")
	}
	args = args[:len(args)-1]
	var maps []string
	codecs := map[int]string{}
	input, filter := false, ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if flagOptions[a] {
			continue
		}
		if !valueOptions[a] || i+1 >= len(args) {
			return j, refuse("option %q", a)
		}
		v := args[i+1]
		i++
		switch a {
		case "-i":
			j.Input, input = v, true
		case "-ss":
			s, err := strconv.ParseFloat(v, 64)
			if err != nil || s < 0 {
				return j, refuse("-ss %q", v)
			}
			j.Start = time.Duration(s * float64(time.Second))
		case "-codec:0", "-codec:1":
			if input { // before -i they name Plex's decoders, which ffgo replaces
				codecs[int(a[len(a)-1]-'0')] = v
			}
		case "-map":
			maps = append(maps, v)
		case "-filter_complex":
			filter = v
		case "-metadata:s:1":
			if lang, ok := strings.CutPrefix(v, "language="); ok {
				j.AudioLanguage = lang
			}
		case "-b:1":
			n, err := bitRate(v)
			if err != nil {
				return j, refuse("-b:1 %q", v)
			}
			j.AudioBitRate = n
		case "-f":
			if v != "dash" {
				return j, refuse("format %q", v)
			}
		case "-dash_segment_type":
			if v != "mp4" {
				return j, refuse("segment type %q", v)
			}
		case "-init_seg_name":
			if v != InitName {
				return j, refuse("init name %q", v)
			}
		case "-media_seg_name":
			if v != MediaName {
				return j, refuse("media name %q", v)
			}
		case "-seg_duration":
			s, err := strconv.ParseFloat(v, 64)
			if err != nil || s <= 0 {
				return j, refuse("-seg_duration %q", v)
			}
			j.SegmentDuration = time.Duration(s * float64(time.Second))
		case "-skip_to_segment":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return j, refuse("-skip_to_segment %q", v)
			}
			j.SkipToSegment = n
		case "-progressurl":
			j.ProgressURL = v
		case "-manifest_name":
			j.ManifestURL = v
		}
	}
	if j.Input == "" || j.SegmentDuration == 0 || j.ManifestURL == "" || j.ProgressURL == "" {
		return j, refuse("missing input, segment duration, manifest or progress URL")
	}
	if j.SkipToSegment == 0 {
		j.SkipToSegment = 1
	}
	if len(maps) != 2 {
		return j, refuse("%d outputs, want 2", len(maps))
	}
	if codecs[0] != "copy" {
		return j, refuse("output 0 is %q, not a copy", codecs[0])
	}
	v := streamRef.FindStringSubmatch(maps[0])
	if v == nil {
		return j, refuse("output 0 maps %q", maps[0])
	}
	j.VideoStream, _ = strconv.Atoi(v[1])
	switch {
	case maps[1] == "[0]":
		m := downmix.FindStringSubmatch(filter)
		if m == nil || codecs[1] != "aac" {
			return j, refuse("audio filter %q with codec %q", filter, codecs[1])
		}
		j.AudioStream, _ = strconv.Atoi(m[1])
		j.AudioSampleRate, _ = strconv.Atoi(m[2])
		j.AudioChannels = 2
	case streamRef.MatchString(maps[1]) && filter == "":
		j.AudioStream, _ = strconv.Atoi(streamRef.FindStringSubmatch(maps[1])[1])
		switch codecs[1] {
		case "copy":
			j.AudioCopy, j.AudioBitRate = true, 0
		case "aac":
		default:
			return j, refuse("audio codec %q", codecs[1])
		}
	default:
		return j, refuse("output 1 maps %q with filter %q", maps[1], filter)
	}
	if !j.AudioCopy && j.AudioBitRate == 0 {
		return j, refuse("an AAC output with no bit rate")
	}
	return j, nil
}

// bitRate reads ffmpeg's "256k", "1M" or a plain number.
func bitRate(v string) (int64, error) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(v, "k"):
		mult, v = 1000, strings.TrimSuffix(v, "k")
	case strings.HasSuffix(v, "M"):
		mult, v = 1000000, strings.TrimSuffix(v, "M")
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n * mult, err
}
