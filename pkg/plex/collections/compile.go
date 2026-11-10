package collections

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/mediactl/clusterplex/pkg/plex/api"
)

var (
	// ErrInvalidRule is a filter Plex cannot take: an unknown field, an
	// operator the field's type lacks, a missing value, a malformed rule.
	ErrInvalidRule = errors.New("invalid rule")
	// ErrUnknownValue is a tag name the library does not hold.
	ErrUnknownValue = errors.New("unknown value")
)

// Resolver gives a tag field's value id on this server for its name
// ("Anime" -> "6437"); ("", nil) when the library holds no such tag.
type Resolver func(field, name string) (id string, err error)

// Carry is what the collection's current filter holds that a spec may
// leave to Plex: its sort and limit.
type Carry struct {
	Sort, Limit string
}

// symbols are Plex's operator symbols by operator word and field type
// (the design's operator table; the Meta's FieldType[].Operator[]).
var symbols = map[string]map[string]string{
	"is":             {"tag": "=", "string": "==", "integer": "=", "audioLanguage": "=", "subtitleLanguage": "=", "resolution": "="},
	"isNot":          {"tag": "!=", "string": "!==", "integer": "!=", "audioLanguage": "!=", "subtitleLanguage": "!="},
	"contains":       {"string": "="},
	"doesNotContain": {"string": "!="},
	"beginsWith":     {"string": "<="},
	"endsWith":       {"string": ">="},
	"greaterThan":    {"integer": ">>="},
	"lessThan":       {"integer": "<<="},
	"before":         {"date": "<<="},
	"after":          {"date": ">>="},
	"isTrue":         {"boolean": "="},
	"isFalse":        {"boolean": "!="},
}

// pair is one key=value of a filter, decoded. A condition's key is its
// field and its operator's symbol but the last "=" ("title=" for "==").
type pair struct{ key, value string }

var (
	push = pair{"push", "1"}
	pop  = pair{"pop", "1"}
)

// ItemTypeNumber is Plex's number for an item type, 0 for none.
func ItemTypeNumber(t string) int {
	return map[string]int{"movie": 1, "show": 2, "season": 3, "episode": 4}[t]
}

// Compile builds a smart collection's filter query as Plex Web does: the
// type, the sort (the spec's, else carry's), the limit (the spec's, else
// carry's), then the filter -- Plex Web's DirectoryListAdvancedFilters
// (4.162.1) joins {type, sort, group, having, limit} before the rules. The query is encoded as Plex Web encodes it --
// every key and value percent-encoded on its own, so a value's "&" or "="
// never splits it -- and Decode reads it.
func Compile(f Filter, sort *Sort, limit *int32, itemType string, m api.SectionMeta, resolve Resolver, carry Carry) (string, error) {
	n := ItemTypeNumber(itemType)
	if n == 0 {
		return "", fmt.Errorf("%w: itemType: %q is not movie, show, season or episode", ErrInvalidRule, itemType)
	}
	ps := []pair{{"type", strconv.Itoa(n)}}
	switch {
	case sort != nil:
		s := sort.Field
		if sort.Descending {
			s += ":desc"
		}
		ps = append(ps, pair{"sort", s})
	case carry.Sort != "":
		ps = append(ps, pair{"sort", carry.Sort})
	}
	switch {
	case limit != nil:
		ps = append(ps, pair{"limit", strconv.Itoa(int(*limit))})
	case carry.Limit != "":
		ps = append(ps, pair{"limit", carry.Limit})
	}
	body, err := compileFilter(f, m, resolve)
	if err != nil {
		return "", err
	}
	return encode(append(ps, body...)), nil
}

// compileFilter is the filter's body: a top level of match all holding
// only conditions joins them; any other top level wraps each entry, and
// itself, in push/pop.
func compileFilter(f Filter, m api.SectionMeta, resolve Resolver) ([]pair, error) {
	join, err := joiner(f.Match, "filter.match")
	if err != nil {
		return nil, err
	}
	if len(f.Rules) == 0 {
		return nil, fmt.Errorf("%w: filter.rules: none", ErrInvalidRule)
	}
	simple := join.key == "and"
	for _, r := range f.Rules {
		if r.Match != "" || len(r.Rules) > 0 {
			simple = false
		}
	}
	var out []pair
	if !simple {
		out = append(out, push)
	}
	for i, r := range f.Rules {
		path := fmt.Sprintf("filter.rules[%d]", i)
		if i > 0 {
			out = append(out, join)
		}
		entry, err := compileRule(r, path, m, resolve)
		if err != nil {
			return nil, err
		}
		if simple {
			out = append(out, entry...)
			continue
		}
		out = append(out, push)
		out = append(out, entry...)
		out = append(out, pop)
	}
	if !simple {
		out = append(out, pop)
	}
	return out, nil
}

// compileRule is a condition's pair, or a group's conditions joined.
func compileRule(r Rule, path string, m api.SectionMeta, resolve Resolver) ([]pair, error) {
	isGroup := r.Match != "" || len(r.Rules) > 0
	isCondition := r.Field != "" || r.Operator != "" || r.Value != ""
	switch {
	case isGroup && isCondition:
		return nil, fmt.Errorf("%w: %s: a rule is a condition or a group, not both", ErrInvalidRule, path)
	case !isGroup && !isCondition:
		return nil, fmt.Errorf("%w: %s: empty", ErrInvalidRule, path)
	case isCondition:
		p, err := condition(Condition{Field: r.Field, Operator: r.Operator, Value: r.Value}, path, m, resolve)
		return []pair{p}, err
	}
	join, err := joiner(r.Match, path+".match")
	if err != nil {
		return nil, err
	}
	if len(r.Rules) == 0 {
		return nil, fmt.Errorf("%w: %s.rules: none", ErrInvalidRule, path)
	}
	var out []pair
	for j, c := range r.Rules {
		if j > 0 {
			out = append(out, join)
		}
		p, err := condition(c, fmt.Sprintf("%s.rules[%d]", path, j), m, resolve)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// joiner is the pair between a level's entries: and=1 for all (the
// default), or=1 for any.
func joiner(match, path string) (pair, error) {
	switch match {
	case "", "all":
		return pair{"and", "1"}, nil
	case "any":
		return pair{"or", "1"}, nil
	}
	return pair{}, fmt.Errorf("%w: %s: %q is not all or any", ErrInvalidRule, path, match)
}

func condition(c Condition, path string, m api.SectionMeta, resolve Resolver) (pair, error) {
	ft, ok := m.Fields[c.Field]
	if !ok {
		return pair{}, fmt.Errorf("%w: %s: the library has no field %q", ErrInvalidRule, path, c.Field)
	}
	sym, ok := symbols[c.Operator][ft]
	if !ok {
		return pair{}, fmt.Errorf("%w: %s: %s (a %s field) does not take %q", ErrInvalidRule, path, c.Field, ft, c.Operator)
	}
	v := strings.TrimSpace(c.Value)
	switch {
	case ft == "boolean":
		v = "1"
	case v == "":
		return pair{}, fmt.Errorf("%w: %s: %s %s needs a value", ErrInvalidRule, path, c.Field, c.Operator)
	case ft == "tag":
		id, err := resolve(c.Field, v)
		if err != nil {
			return pair{}, fmt.Errorf("%s: %s %q: %w", path, c.Field, v, err)
		}
		if id == "" {
			return pair{}, fmt.Errorf("%w: %s: the library has no %s %q", ErrUnknownValue, path, c.Field, v)
		}
		v = id
	}
	return pair{c.Field + strings.TrimSuffix(sym, "="), v}, nil
}

// encode joins the pairs, each key and value percent-encoded as
// JavaScript's encodeURIComponent does, which is how Plex Web stores a
// filter ("title%3D=Seven%20Up!").
func encode(ps []pair) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = encodeComponent(p.key) + "=" + encodeComponent(p.value)
	}
	return strings.Join(parts, "&")
}

// encodeComponent is encodeURIComponent: everything but A-Z a-z 0-9 and
// - _ . ! ~ * ' ( ) is percent-encoded, byte by byte in UTF-8.
func encodeComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

// pairs splits an encoded query into its decoded pairs; a part that does
// not decode is kept as it is.
func pairs(query string) []pair {
	if query == "" {
		return nil
	}
	var ps []pair
	for _, part := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(part, "=")
		if dk, err := url.PathUnescape(k); err == nil {
			k = dk
		}
		if dv, err := url.PathUnescape(v); err == nil {
			v = dv
		}
		ps = append(ps, pair{k, v})
	}
	return ps
}

// Decode is an encoded query as a person reads it
// ("type=1&push=1&title==Seven Up!&..."), for status and logs. It is not
// a query: a value's "&" reads as it is.
func Decode(query string) string {
	ps := pairs(query)
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.key + "=" + p.value
	}
	return strings.Join(parts, "&")
}

// ParseContent reads a smart collection's content (its server:// URI):
// the query, re-encoded as Compile encodes, so a filter Plex Web wrote
// compares equal to the same filter compiled, and the sort and limit it
// holds.
func ParseContent(content string) (string, Carry) {
	_, raw, ok := strings.Cut(content, "?")
	if !ok {
		return "", Carry{}
	}
	ps := pairs(raw)
	var carry Carry
	for _, p := range ps {
		switch p.key {
		case "sort":
			carry.Sort = p.value
		case "limit":
			carry.Limit = p.value
		}
	}
	return encode(ps), carry
}

const libraryProvider = "/com.plexapp.plugins.library/library/"

// SmartURI is a smart collection's filter URI for a section on this
// server: the query, already encoded, after Plex's prefix.
func SmartURI(machineID, sectionKey, query string) string {
	return "server://" + machineID + libraryProvider + "sections/" + sectionKey + "/all?" + query
}

// ItemsURI names items by rating key, as a normal collection's items.
func ItemsURI(machineID string, ratingKeys []string) string {
	return "server://" + machineID + libraryProvider + "metadata/" + strings.Join(ratingKeys, ",")
}
