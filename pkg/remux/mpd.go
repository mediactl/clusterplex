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
	Start, Now      time.Time
	Video, Audio    Representation
}

func seconds(d time.Duration) string { return fmt.Sprintf("PT%.1fS", d.Seconds()) }

func (r Representation) longest() time.Duration {
	var max int64
	for _, e := range r.Timeline {
		max = max(max, e.D)
	}
	if r.Timescale == 0 {
		return 0
	}
	return time.Duration(max) * time.Second / time.Duration(r.Timescale)
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
		fmt.Fprintf(&b, "\ttype=\"dynamic\"\n\tminimumUpdatePeriod=%q\n\tsuggestedPresentationDelay=%q\n", seconds(2*m.SegmentDuration), seconds(2*m.SegmentDuration))
		fmt.Fprintf(&b, "\tavailabilityStartTime=%q\n\tpublishTime=%q\n", m.Start.UTC().Format("2006-01-02T15:04:05.000Z"), m.Now.UTC().Format("2006-01-02T15:04:05.000Z"))
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
	fmt.Fprintf(&b, "\t\t<AdaptationSet id=\"1\" contentType=\"audio\" startWithSAP=\"1\" segmentAlignment=\"true\" bitstreamSwitching=\"true\" lang=%q>\n", a.Language)
	fmt.Fprintf(&b, "\t\t\t<Representation id=\"1\" mimeType=\"audio/mp4\" codecs=%q bandwidth=\"%d\" audioSamplingRate=\"%d\">\n", a.Codecs, a.Bandwidth, a.SampleRate)
	fmt.Fprintf(&b, "\t\t\t\t<AudioChannelConfiguration schemeIdUri=\"urn:mpeg:dash:23003:3:audio_channel_configuration:2011\" value=\"%d\" />\n", a.Channels)
	m.template(&b, a)
	b.WriteString("\t\t\t</Representation>\n\t\t</AdaptationSet>\n\t</Period>\n</MPD>\n")
	return b.Bytes()
}

func (m Manifest) template(b *bytes.Buffer, r Representation) {
	fmt.Fprintf(b, "\t\t\t\t<SegmentTemplate timescale=\"%d\" initialization=%q media=%q startNumber=\"%d\">\n\t\t\t\t\t<SegmentTimeline>\n",
		r.Timescale, InitName, MediaName, m.StartNumber)
	next := int64(-1)
	for _, e := range r.Timeline {
		if e.T != next {
			fmt.Fprintf(b, "\t\t\t\t\t\t<S t=\"%d\" d=\"%d\" />\n", e.T, e.D)
		} else {
			fmt.Fprintf(b, "\t\t\t\t\t\t<S d=\"%d\" />\n", e.D)
		}
		next = e.T + e.D
	}
	b.WriteString("\t\t\t\t\t</SegmentTimeline>\n\t\t\t\t</SegmentTemplate>\n")
}
