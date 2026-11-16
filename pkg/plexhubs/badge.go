package plexhubs

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// Badge is which of an airing's marks the proxy takes off when the guide
// does not call the airing live.
//
// PMS marks every airing that is on right now onAir, on the item and on its
// Media, and an episode it takes for a first showing premiere; it has no
// mark for a live broadcast, and Plex's iOS and Apple TV apps draw a red
// LIVE badge on What's On rows from one of these (owner, 2026-11-16: every
// item on now carried it). cluster-plex's guide build writes the guide's own
// <live/> into the airing (at:live, which PMS answers as the Media's
// live="1"), so the proxy can tell the two apart. Which mark the apps read is
// not known, so each can be taken off on its own, and nothing is by default.
type Badge struct {
	// ItemOnAir and MediaOnAir take onAir off the item and off its Media.
	ItemOnAir, MediaOnAir bool
	// Premiere takes premiere off the Media.
	Premiere bool
}

// Off reports whether nothing is taken off.
func (b Badge) Off() bool { return b == Badge{} }

// ParseBadge reads a comma-separated list of marks: item-onair, media-onair,
// onair (both) and premiere. Anything else, and the empty string, is none.
func ParseBadge(s string) Badge {
	var b Badge
	for _, name := range strings.Split(s, ",") {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "item-onair":
			b.ItemOnAir = true
		case "media-onair":
			b.MediaOnAir = true
		case "onair":
			b.ItemOnAir, b.MediaOnAir = true, true
		case "premiere":
			b.Premiere = true
		}
	}
	return b
}

// StripJSON takes b's marks off every airing of a JSON answer the guide does
// not call live; false when body is not JSON or nothing changed.
func StripJSON(body []byte, b Badge) ([]byte, bool) {
	if b.Off() {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if dec.Decode(&doc) != nil {
		return nil, false
	}
	if !stripValue(doc, b) {
		return nil, false
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if enc.Encode(doc) != nil {
		return nil, false
	}
	return bytes.TrimRight(out.Bytes(), "\n"), true
}

// stripValue walks a decoded answer: every object with a Media list is an
// item, wherever it sits (a hub's Metadata, a channel's, a list's).
func stripValue(v any, b Badge) bool {
	changed := false
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			changed = stripValue(e, b) || changed
		}
	case map[string]any:
		if media, ok := v["Media"].([]any); ok {
			changed = stripItem(v, media, b) || changed
		}
		for _, e := range v {
			changed = stripValue(e, b) || changed
		}
	}
	return changed
}

func stripItem(item map[string]any, media []any, b Badge) bool {
	changed, live := false, false
	for _, m := range media {
		m, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if isLive(m["live"]) {
			live = true
			continue
		}
		if _, on := m["onAir"]; on && b.MediaOnAir {
			delete(m, "onAir")
			changed = true
		}
		if _, first := m["premiere"]; first && b.Premiere {
			delete(m, "premiere")
			changed = true
		}
	}
	if _, on := item["onAir"]; on && b.ItemOnAir && !live {
		delete(item, "onAir")
		changed = true
	}
	return changed
}

// isLive reads a Media's live attribute, which PMS writes as the string the
// guide stored.
func isLive(v any) bool {
	switch v := v.(type) {
	case string:
		return v == "1" || v == "true"
	case bool:
		return v
	case json.Number:
		return v.String() == "1"
	}
	return false
}

var (
	// xmlItem is an item of an XML answer, with its Media inside: PMS
	// writes no item within another.
	xmlItem  = regexp.MustCompile(`(?s)<(Video|Directory|Track|Photo)\b[^>]*?(?:/>|>.*?</(?:Video|Directory|Track|Photo)>)`)
	xmlOpen  = regexp.MustCompile(`^<\w+\b[^>]*>`)
	xmlMedia = regexp.MustCompile(`<Media\b[^>]*>`)
	xmlLive  = regexp.MustCompile(` live="(?:1|true)"`)
	xmlOnAir = regexp.MustCompile(` onAir="[^"]*"`)
	xmlFirst = regexp.MustCompile(` premiere="[^"]*"`)
)

// StripXML is StripJSON for an XML answer.
func StripXML(body []byte, b Badge) ([]byte, bool) {
	if b.Off() {
		return nil, false
	}
	changed := false
	out := xmlItem.ReplaceAllFunc(body, func(item []byte) []byte {
		live := false
		fixed := xmlMedia.ReplaceAllFunc(item, func(media []byte) []byte {
			if xmlLive.Match(media) {
				live = true
				return media
			}
			if b.MediaOnAir {
				media = xmlOnAir.ReplaceAll(media, nil)
			}
			if b.Premiere {
				media = xmlFirst.ReplaceAll(media, nil)
			}
			return media
		})
		if b.ItemOnAir && !live {
			if open := xmlOpen.Find(fixed); open != nil {
				fixed = append(xmlOnAir.ReplaceAll(open, nil), fixed[len(open):]...)
			}
		}
		if !bytes.Equal(fixed, item) {
			changed = true
		}
		return fixed
	})
	return out, changed
}
