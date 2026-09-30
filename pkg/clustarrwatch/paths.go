package clustarrwatch

import (
	"path"
	"slices"
	"strings"
)

// Mapping is one prefix of clustarr's paths and where Plex sees it.
type Mapping struct {
	Clustarr string `mapstructure:"clustarr"`
	Plex     string `mapstructure:"plex"`
}

// Mapper translates clustarr's paths to Plex's, longest prefix first.
type Mapper struct{ ms []Mapping }

// NewMapper cleans the mappings and orders them longest prefix first.
func NewMapper(ms []Mapping) Mapper {
	out := make([]Mapping, len(ms))
	for i, m := range ms {
		out[i] = Mapping{Clustarr: path.Clean(m.Clustarr), Plex: path.Clean(m.Plex)}
	}
	slices.SortStableFunc(out, func(a, b Mapping) int { return len(b.Clustarr) - len(a.Clustarr) })
	return Mapper{ms: out}
}

// Map returns p as Plex sees it. A prefix matches whole path elements only.
func (m Mapper) Map(p string) (string, bool) {
	p = path.Clean(p)
	for _, mp := range m.ms {
		if mp.Clustarr == "/" {
			return path.Join(mp.Plex, p), true
		}
		if p == mp.Clustarr || strings.HasPrefix(p, mp.Clustarr+"/") {
			return path.Join(mp.Plex, strings.TrimPrefix(p, mp.Clustarr)), true
		}
	}
	return "", false
}
