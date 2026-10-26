package video

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// valueOptions take one argument, the per-stream ones (-codec:0, -b:1,
// -metadata:s:1) named without their stream specifier; flagOptions take
// none. Both are what captured conversions carry (testdata): anything else
// is refused, so a Plex release that adds an option is refused by name
// rather than run on a guess.
var valueOptions = map[string]bool{
	"-codec": true, "-analyzeduration": true, "-probesize": true, "-eae_prefix": true,
	"-ss": true, "-i": true, "-fps_mode": true, "-loglevel": true, "-loglevel_plex": true,
	"-progressurl": true, "-filter_complex": true, "-map": true,
	"-crf": true, "-maxrate": true, "-bufsize": true, "-r": true, "-preset": true, "-x264opts": true,
	"-force_key_frames": true, "-metadata": true, "-b": true,
	"-segment_format": true, "-f": true, "-individual_header_trailer": true,
	"-segment_time": true, "-segment_start_number": true, "-segment_time_delta": true,
	"-segment_list": true, "-segment_list_type": true, "-segment_list_size": true,
	"-segment_list_separate_stream_times": true, "-segment_list_unfinished": true,
	"-max_delay": true, "-avoid_negative_ts": true, "-map_metadata": true, "-map_chapters": true,
}

var flagOptions = map[string]bool{
	"-start_at_zero": true, "-copyts": true, "-y": true, "-nostats": true,
}

// perOutput are the options whose specifier is an output stream's index.
var perOutput = map[string]bool{
	"-codec": true, "-crf": true, "-maxrate": true, "-bufsize": true, "-r": true,
	"-preset": true, "-x264opts": true, "-force_key_frames": true, "-b": true,
}

// fixed are options whose one captured value Plex's segmenting relies on:
// another is a job the pool cannot reproduce.
var fixed = map[string]string{
	"-fps_mode": "cfr", "-individual_header_trailer": "0", "-segment_list_type": "csv",
	"-segment_list_separate_stream_times": "1", "-segment_list_unfinished": "1",
	"-avoid_negative_ts": "disabled", "-map_metadata": "-1", "-map_chapters": "-1",
}

// videoEncoders are the encoders whose output the pool makes: H.264, which
// every captured conversion asks for (the spec's non-goal: no HEVC or AV1).
var videoEncoders = map[string]bool{"libx264": true}

var (
	streamRef     = regexp.MustCompile(`^0:(\d+)$`)
	keyframeExpr  = regexp.MustCompile(`^expr:gte\(t,n_forced\*([0-9]+(?:\.[0-9]+)?)\)$`)
	segmentPatten = regexp.MustCompile(`^media-%05d\.ts$`)
)

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrShape, fmt.Sprintf(format, a...))
}

// output is one -map and what the argv says of it.
type output struct {
	src     int      // the input stream it is made from
	filters []filter // the -filter_complex chain it went through, in order
	opts    map[string]string
}

// Parse reads a Plex Transcoder argv (without argv[0]) and the job's
// environment. It accepts the shapes captured conversions take, and wraps
// ErrShape, naming what it refused, for anything else.
func Parse(args []string, env map[string]string) (Job, error) {
	j := Job{Token: env["X_PLEX_TOKEN"]}
	if len(args) < 2 || strings.HasPrefix(args[len(args)-1], "-") {
		return j, refuse("no output")
	}
	target := args[len(args)-1]
	args = args[:len(args)-1]
	last := lastInput(args)
	var maps []string
	var chains []chain
	opts := map[string]string{}
	perOut := map[int]map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if flagOptions[a] {
			continue
		}
		base, spec, _ := strings.Cut(a, ":")
		if !valueOptions[base] || i+1 >= len(args) {
			return j, refuse("option %q", a)
		}
		v := args[i+1]
		i++
		input := i < last // an option before the last -i is an input option
		switch {
		case base == "-i":
			in, err := inputOf(v)
			if err != nil {
				return j, err
			}
			j.Inputs = append(j.Inputs, in)
		case base == "-ss":
			if !input {
				return j, refuse("-ss after the input")
			}
			s, err := strconv.ParseFloat(v, 64)
			if err != nil || s < 0 {
				return j, refuse("-ss %q", v)
			}
			j.Start = time.Duration(s * float64(time.Second))
		case input && (base == "-codec" || base == "-eae_prefix"):
			// Before -i they name Plex's decoders (eac3_eae is its
			// EasyAudioEncoder); the pool decodes with FFmpeg's own.
		case base == "-analyzeduration" || base == "-probesize" || base == "-loglevel" ||
			base == "-loglevel_plex" || base == "-max_delay" || base == "-preset" || base == "-x264opts":
			// Read and ignored: probing and logging are the pool's own, and
			// x264's tuning has no QSV meaning.
		case base == "-metadata":
			n, ok := strings.CutPrefix(spec, "s:")
			idx, err := strconv.Atoi(n)
			if !ok || err != nil {
				return j, refuse("option %q", a)
			}
			lang, ok := strings.CutPrefix(v, "language=")
			if !ok {
				return j, refuse("metadata %q", v)
			}
			perOutOf(perOut, idx)["language"] = lang
		case perOutput[base]:
			idx, err := strconv.Atoi(spec)
			if err != nil {
				return j, refuse("option %q", a)
			}
			perOutOf(perOut, idx)[base] = v
		case base == "-map":
			maps = append(maps, v)
		case base == "-filter_complex":
			cs, err := parseFilterComplex(v)
			if err != nil {
				return j, err
			}
			chains = append(chains, cs...)
		default:
			if spec != "" {
				return j, refuse("option %q", a)
			}
			if want, ok := fixed[base]; ok && v != want {
				return j, refuse("%s %q", base, v)
			}
			opts[base] = v
		}
	}
	if len(j.Inputs) == 0 {
		return j, refuse("no input")
	}
	j.ProgressURL, j.SegmentListURL = opts["-progressurl"], opts["-segment_list"]
	j.Session = sessionOf(j.ProgressURL)
	if j.ProgressURL == "" || j.Session == "" {
		return j, refuse("no progress URL naming a session")
	}
	if err := j.outputs(maps, chains, perOut); err != nil {
		return j, err
	}
	return j, j.muxer(opts, target)
}

func perOutOf(m map[int]map[string]string, idx int) map[string]string {
	if m[idx] == nil {
		m[idx] = map[string]string{}
	}
	return m[idx]
}

// lastInput is the index of the last -i: the options before it are input
// options, the rest output options.
func lastInput(args []string) int {
	for i := len(args) - 1; i >= 0; i-- {
		if args[i] == "-i" {
			return i
		}
	}
	return -1
}

// inputOf is a library file under /media, read by the worker itself, or
// anything else (Plex's live session, a subtitle in Plex's own files),
// which the manager streams.
func inputOf(v string) (Input, error) {
	if v == "" {
		return Input{}, refuse("an empty input")
	}
	if strings.HasPrefix(v, "/media/") && !strings.Contains(v, "/../") {
		return Input{Path: v}, nil
	}
	name := ""
	if !strings.Contains(v, "://") {
		name = v[strings.LastIndex(v, "/")+1:]
	}
	return Input{Streamed: true, Source: v, Name: name}, nil
}

// outputs reads each -map: one video stream encoded to H.264 through
// Plex's scale and format filters, and at most one audio stream, copied or
// encoded to AAC through its resampler.
func (j *Job) outputs(maps []string, chains []chain, perOut map[int]map[string]string) error {
	outs := make([]output, len(maps))
	for i, m := range maps {
		src, filters, err := resolve(m, chains)
		if err != nil {
			return err
		}
		outs[i] = output{src: src, filters: filters, opts: perOut[i]}
		if outs[i].opts == nil {
			outs[i].opts = map[string]string{}
		}
	}
	for idx := range perOut {
		if idx < 0 || idx >= len(outs) {
			return refuse("options for output %d of %d", idx, len(outs))
		}
	}
	video := -1
	for i, o := range outs {
		codec := o.opts["-codec"]
		switch {
		case codec == "":
			return refuse("output %d has no codec", i)
		case videoEncoders[codec]:
			if video >= 0 {
				return refuse("a second video output")
			}
			video = i
			if err := j.videoOut(o); err != nil {
				return err
			}
		case codec == "aac" || codec == "copy":
			if j.Audio != nil {
				return refuse("a second audio output")
			}
			a, err := audioOut(o)
			if err != nil {
				return err
			}
			j.Audio = &a
		default:
			return refuse("codec %q for output %d", codec, i)
		}
	}
	if video < 0 {
		return refuse("no H.264 video output")
	}
	return nil
}

func (j *Job) videoOut(o output) error {
	v := VideoOut{Stream: o.src}
	for _, f := range o.filters {
		switch f.name {
		case "scale":
			for _, kv := range f.args {
				switch kv.k {
				case "w", "h":
					n, err := strconv.Atoi(kv.v)
					if err != nil || n <= 0 {
						return refuse("scale %s=%q", kv.k, kv.v)
					}
					if kv.k == "w" {
						v.Width = n
					} else {
						v.Height = n
					}
				case "force_divisible_by":
				default:
					return refuse("scale option %q", kv.k)
				}
			}
		case "format":
			fmts := ""
			for _, kv := range f.args {
				if kv.k != "pix_fmts" {
					return refuse("format option %q", kv.k)
				}
				fmts = kv.v
			}
			pf, err := pixFmt(fmts)
			if err != nil {
				return err
			}
			v.PixFmt = pf
		default:
			return refuse("video filter %q", f.name)
		}
	}
	if v.Width == 0 || v.Height == 0 || v.PixFmt == "" {
		return refuse("a video output without Plex's scale and format")
	}
	for k, val := range o.opts {
		var err error
		switch k {
		case "-codec", "-preset", "-x264opts":
		case "-crf":
			v.CRF, err = strconv.Atoi(val)
		case "-maxrate":
			v.MaxRate, err = bitRate(val)
		case "-bufsize":
			v.BufSize, err = bitRate(val)
		case "-r":
			v.FrameRate, err = frameRate(val)
		case "-force_key_frames":
			m := keyframeExpr.FindStringSubmatch(val)
			if m == nil {
				return refuse("-force_key_frames %q", val)
			}
			s, _ := strconv.ParseFloat(m[1], 64)
			v.KeyframeEvery = time.Duration(s * float64(time.Second))
		default:
			return refuse("video option %s", k)
		}
		if err != nil {
			return refuse("%s %q", k, val)
		}
	}
	if v.FrameRate.Den == 0 || v.KeyframeEvery <= 0 || v.MaxRate <= 0 || v.BufSize <= 0 {
		return refuse("a video output without a frame rate, forced keyframes, maxrate and bufsize")
	}
	j.Video = v
	return nil
}

// pixFmt is the format the encoder is given from Plex's allowed list: nv12,
// which QSV encodes, or yuv420p; both are the same 8-bit 4:2:0 to a player.
func pixFmt(list string) (string, error) {
	fmts := strings.Split(list, "|")
	for _, want := range []string{"nv12", "yuv420p"} {
		if slices.Contains(fmts, want) {
			return want, nil
		}
	}
	return "", refuse("pixel formats %q", list)
}

func audioOut(o output) (AudioOut, error) {
	a := AudioOut{Stream: o.src, Copy: o.opts["-codec"] == "copy", Language: o.opts["language"]}
	for _, f := range o.filters {
		if a.Copy || f.name != "aresample" {
			return a, refuse("audio filter %q", f.name)
		}
		for _, kv := range f.args {
			switch kv.k {
			case "async", "rematrix_maxval":
			case "ochl":
				switch kv.v {
				case "stereo":
					a.Channels = 2
				case "mono":
					a.Channels = 1
				default:
					return a, refuse("channel layout %q", kv.v)
				}
			case "osr":
				n, err := strconv.Atoi(kv.v)
				if err != nil || n <= 0 {
					return a, refuse("osr %q", kv.v)
				}
				a.SampleRate = n
			default:
				return a, refuse("aresample option %q", kv.k)
			}
		}
	}
	for k, val := range o.opts {
		switch k {
		case "-codec", "language":
		case "-b":
			n, err := bitRate(val)
			if err != nil {
				return a, refuse("-b %q", val)
			}
			a.BitRate = n
		default:
			return a, refuse("audio option %s", k)
		}
	}
	if !a.Copy && a.BitRate == 0 {
		return a, refuse("an AAC output with no bit rate")
	}
	return a, nil
}

// muxer reads the output's format: Plex's stream segmenter writing
// MPEG-TS segments named media-%05d.ts and listing them in CSV.
func (j *Job) muxer(opts map[string]string, target string) error {
	switch f := opts["-f"]; f {
	case "ssegment":
		if sf := opts["-segment_format"]; sf != "mpegts" {
			return refuse("segment format %q", sf)
		}
		if !segmentPatten.MatchString(target) {
			return refuse("segment pattern %q", target)
		}
		j.Output = OutputHLS
		s := Segmenting{Pattern: target}
		var err error
		if s.Duration, err = seconds(opts["-segment_time"]); err != nil || s.Duration <= 0 {
			return refuse("-segment_time %q", opts["-segment_time"])
		}
		if v, ok := opts["-segment_time_delta"]; ok {
			if s.TimeDelta, err = seconds(v); err != nil || s.TimeDelta < 0 {
				return refuse("-segment_time_delta %q", v)
			}
		}
		if v, ok := opts["-segment_start_number"]; ok {
			if s.StartNumber, err = strconv.Atoi(v); err != nil || s.StartNumber < 0 {
				return refuse("-segment_start_number %q", v)
			}
		}
		if v, ok := opts["-segment_list_size"]; ok {
			if s.ListSize, err = strconv.Atoi(v); err != nil || s.ListSize < 0 {
				return refuse("-segment_list_size %q", v)
			}
		}
		if j.SegmentListURL == "" {
			return refuse("no segment list")
		}
		j.Segments = s
		return nil
	default:
		return refuse("muxer %q", f)
	}
}

func seconds(v string) (time.Duration, error) {
	s, err := strconv.ParseFloat(v, 64)
	return time.Duration(math.Round(s * float64(time.Second))), err
}

// frameRate is -r as the encoder takes it. Plex prints a rational rate as
// a float (23.975999999999999), and Intel's runtime refuses an irregular
// fraction (clustarr, 2026-10-20: hevc_qsv failed -38), so a rate within
// 1e-4 of an NTSC rate is that rate, and any other is read to the
// millihertz.
func frameRate(v string) (Rational, error) {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 || f > 1000 {
		return Rational{}, fmt.Errorf("frame rate %q", v)
	}
	for _, n := range []int{24, 30, 48, 60, 120} {
		ntsc := float64(n) * 1000 / 1001
		if math.Abs(f-ntsc)/ntsc < 1e-4 {
			return Rational{Num: n * 1000, Den: 1001}, nil
		}
	}
	m := int(math.Round(f * 1000))
	g := gcd(m, 1000)
	return Rational{Num: m / g, Den: 1000 / g}, nil
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
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
	if err == nil && n <= 0 {
		err = fmt.Errorf("bit rate %q", v)
	}
	return n * mult, err
}
