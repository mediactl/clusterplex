package remux

import (
	"bytes"
	"fmt"
	"time"
)

type TimelineEntry struct{ T, D int64 }

type Representation struct {
	Codecs               string
	Bandwidth            int64
	Width, Height        int
	FrameRate            string
	SampleRate, Channels int
	Language             string
	Timescale            int32
	Timeline             []TimelineEntry
}

// Manifest is the MPD the worker publishes after each segment: the shape
// Plex's own binary posts (ffmpeg's dashenc), dynamic while the job runs
// and static with the whole duration at its end.
type Manifest struct {
	Final           bool
	Duration        time.Duration
	StartNumber     int
	SegmentDuration time.Duration
	// Window is how many segments a running manifest lists (-window_size),
	// 0 for all of them.
	Window       int
	Start, Now   time.Time
	Video, Audio Representation
}

// seconds is d as ffmpeg's dashenc writes a time (write_time): hours and
// minutes when there are any, then seconds to the tenth, truncated.
func seconds(d time.Duration) string {
	s := int(d / time.Second)
	tenths := int(d%time.Second) / int(100*time.Millisecond)
	h, m := s/3600, s/60%60
	out := "PT"
	if h > 0 {
		out += fmt.Sprintf("%dH", h)
	}
	if h > 0 || m > 0 {
		out += fmt.Sprintf("%dM", m)
	}
	return out + fmt.Sprintf("%d.%dS", s%60, tenths)
}

// last is the duration of a representation's last segment, as dashenc's
// last_duration.
func (r Representation) last() time.Duration {
	if n := len(r.Timeline); n > 0 && r.Timescale > 0 {
		return time.Duration(r.Timeline[n-1].D) * time.Second / time.Duration(r.Timescale)
	}
	return 0
}

func (r Representation) longest() time.Duration {
	var d int64
	for _, e := range r.Timeline {
		d = max(d, e.D)
	}
	if r.Timescale == 0 {
		return 0
	}
	return time.Duration(d) * time.Second / time.Duration(r.Timescale)
}

func (m Manifest) Render() []byte {
	var b bytes.Buffer
	longest := time.Duration(max(int64(m.Video.longest()), int64(m.Audio.longest())))
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<MPD xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"` + "\n")
	b.WriteString("\txmlns=\"urn:mpeg:dash:schema:mpd:2011\"\n\txmlns:xlink=\"http://www.w3.org/1999/xlink\"\n")
	b.WriteString("\txsi:schemaLocation=\"urn:mpeg:DASH:schema:MPD:2011 http://standards.iso.org/ittf/PubliclyAvailableStandards/MPEG-DASH_schema_files/DASH-MPD.xsd\"\n")
	b.WriteString("\tprofiles=\"urn:mpeg:dash:profile:isoff-live:2011\"\n")
	if m.Final {
		fmt.Fprintf(&b, "\ttype=\"static\"\n\tmediaPresentationDuration=%q\n", seconds(m.Duration))
	} else {
		// dashenc's: the update period and the presentation delay its last
		// segment's whole seconds, the time-shift buffer -window_size of them.
		last := m.Video.last()
		if last <= 0 {
			last = m.SegmentDuration
		}
		whole := fmt.Sprintf("PT%dS", int(last/time.Second))
		fmt.Fprintf(&b, "\ttype=\"dynamic\"\n\tminimumUpdatePeriod=%q\n\tsuggestedPresentationDelay=%q\n", whole, whole)
		fmt.Fprintf(&b, "\tavailabilityStartTime=%q\n\tpublishTime=%q\n", m.Start.UTC().Format("2006-01-02T15:04:05.000Z"), m.Now.UTC().Format("2006-01-02T15:04:05.000Z"))
		if m.Window > 0 {
			fmt.Fprintf(&b, "\ttimeShiftBufferDepth=%q\n", seconds(last*time.Duration(m.Window)))
		}
	}
	fmt.Fprintf(&b, "\tmaxSegmentDuration=%q\n\tminBufferTime=%q>\n", seconds(m.SegmentDuration), seconds(2*longest))
	b.WriteString("\t<ProgramInformation>\n\t</ProgramInformation>\n\t<ServiceDescription id=\"0\">\n\t</ServiceDescription>\n")
	b.WriteString("\t<Period id=\"0\" start=\"PT0.0S\">\n")
	v := m.Video
	fmt.Fprintf(&b, "\t\t<AdaptationSet id=\"0\" contentType=\"video\" startWithSAP=\"1\" segmentAlignment=\"true\" bitstreamSwitching=\"true\" frameRate=%q maxWidth=\"%d\" maxHeight=\"%d\" par=\"16:9\">\n", v.FrameRate, v.Width, v.Height)
	fmt.Fprintf(&b, "\t\t\t<Representation id=\"0\" mimeType=\"video/mp4\" codecs=%q bandwidth=\"%d\" width=\"%d\" height=\"%d\" sar=\"1:1\">\n", v.Codecs, v.Bandwidth, v.Width, v.Height)
	m.template(&b, v)
	b.WriteString("\t\t\t</Representation>\n\t\t</AdaptationSet>\n")
	a := m.Audio
	// dashenc names a language only when the stream has one.
	lang := ""
	if a.Language != "" {
		lang = fmt.Sprintf(" lang=%q", a.Language)
	}
	fmt.Fprintf(&b, "\t\t<AdaptationSet id=\"1\" contentType=\"audio\" startWithSAP=\"1\" segmentAlignment=\"true\" bitstreamSwitching=\"true\"%s>\n", lang)
	fmt.Fprintf(&b, "\t\t\t<Representation id=\"1\" mimeType=\"audio/mp4\" codecs=%q bandwidth=\"%d\" audioSamplingRate=\"%d\">\n", a.Codecs, a.Bandwidth, a.SampleRate)
	fmt.Fprintf(&b, "\t\t\t\t<AudioChannelConfiguration schemeIdUri=\"urn:mpeg:dash:23003:3:audio_channel_configuration:2011\" value=\"%d\" />\n", a.Channels)
	m.template(&b, a)
	b.WriteString("\t\t\t</Representation>\n\t\t</AdaptationSet>\n\t</Period>\n</MPD>\n")
	return b.Bytes()
}

func (m Manifest) template(b *bytes.Buffer, r Representation) {
	fmt.Fprintf(b, "\t\t\t\t<SegmentTemplate timescale=\"%d\" initialization=%q media=%q startNumber=\"%d\">\n\t\t\t\t\t<SegmentTimeline>\n",
		r.Timescale, InitName, MediaName, m.StartNumber)
	// dashenc's: t where the timeline does not follow on, and r for each
	// following segment of the same duration that does.
	next := int64(-1)
	for i := 0; i < len(r.Timeline); {
		e := r.Timeline[i]
		repeat := 0
		for i+repeat+1 < len(r.Timeline) {
			f, prev := r.Timeline[i+repeat+1], r.Timeline[i+repeat]
			if f.D != e.D || f.T != prev.T+prev.D {
				break
			}
			repeat++
		}
		b.WriteString("\t\t\t\t\t\t<S ")
		if e.T != next {
			fmt.Fprintf(b, "t=\"%d\" ", e.T)
		}
		fmt.Fprintf(b, "d=\"%d\" ", e.D)
		if repeat > 0 {
			fmt.Fprintf(b, "r=\"%d\" ", repeat)
		}
		b.WriteString("/>\n")
		next = e.T + int64(repeat+1)*e.D
		i += repeat + 1
	}
	b.WriteString("\t\t\t\t\t</SegmentTimeline>\n\t\t\t\t</SegmentTemplate>\n")
}
