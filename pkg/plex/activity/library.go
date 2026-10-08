package activity

import (
	"context"
	"net/url"
	"strconv"
)

// Server is what Plex says about itself at /.
type Server struct {
	Version  string
	Platform string
}

// ReadServer reads Plex's version and platform.
func ReadServer(ctx context.Context, pms PMS) (Server, error) {
	var out struct {
		MediaContainer struct {
			Version  string `json:"version"`
			Platform string `json:"platform"`
		} `json:"MediaContainer"`
	}
	if err := pms.Get(ctx, "/", nil, &out); err != nil {
		return Server{}, err
	}
	return Server{Version: out.MediaContainer.Version, Platform: out.MediaContainer.Platform}, nil
}

// Count is how many items of one type a library section holds.
type Count struct {
	Section     string // the section's title
	SectionType string // movie, show, artist or photo
	Type        string // movie, show, season, episode, artist, album, track or photo
	Items       int64
}

// sectionTypes is what Tautulli counts per kind of library -- its count,
// parent count and child count -- by Plex's metadata type numbers.
var sectionTypes = map[string][]struct {
	name string
	num  int
}{
	"movie":  {{"movie", 1}},
	"show":   {{"show", 2}, {"season", 3}, {"episode", 4}},
	"artist": {{"artist", 8}, {"album", 9}, {"track", 10}},
	"photo":  {{"photo", 13}},
}

// ReadLibrary counts every section's items. A section whose count fails is
// left out and the first error returned beside what was counted, so one
// broken library does not hide the others.
func ReadLibrary(ctx context.Context, pms PMS) ([]Count, error) {
	var sections struct {
		MediaContainer struct {
			Directory []struct {
				Key   flexString `json:"key"`
				Type  string     `json:"type"`
				Title string     `json:"title"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := pms.Get(ctx, "/library/sections/all", nil, &sections); err != nil {
		return nil, err
	}
	var (
		out   []Count
		first error
	)
	for _, sec := range sections.MediaContainer.Directory {
		for _, t := range sectionTypes[sec.Type] {
			n, err := countItems(ctx, pms, string(sec.Key), t.num)
			if err != nil {
				if first == nil {
					first = err
				}
				continue
			}
			out = append(out, Count{Section: sec.Title, SectionType: sec.Type, Type: t.name, Items: n})
		}
	}
	return out, first
}

// countItems asks for no items of a type, only their total, as
// python-plexapi's totalViewSize does.
func countItems(ctx context.Context, pms PMS, section string, typ int) (int64, error) {
	q := url.Values{}
	q.Set("type", strconv.Itoa(typ))
	q.Set("includeCollections", "0")
	q.Set("X-Plex-Container-Start", "0")
	q.Set("X-Plex-Container-Size", "0")
	var out struct {
		MediaContainer struct {
			TotalSize flexInt `json:"totalSize"`
			Size      flexInt `json:"size"`
		} `json:"MediaContainer"`
	}
	if err := pms.Get(ctx, "/library/sections/"+url.PathEscape(section)+"/all", q, &out); err != nil {
		return 0, err
	}
	return int64(out.MediaContainer.TotalSize), nil
}
