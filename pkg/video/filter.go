package video

import (
	"regexp"
	"strconv"
	"strings"
)

// kv is one filter argument: key=value, or a positional value with no key.
type kv struct{ k, v string }

// filter is one filter of a chain, its arguments in order.
type filter struct {
	name string
	args []kv
}

// chain is one ;-separated part of a -filter_complex: its input labels,
// its filters and its output label, e.g.
// [0:0]scale=w=720:h=404:force_divisible_by=4[0].
type chain struct {
	in      []string
	filters []filter
	out     string
}

var (
	chainShape = regexp.MustCompile(`^\s*((?:\[[^\]]*\]\s*)+)(.*?)\s*\[([^\]]+)\]\s*$`)
	label      = regexp.MustCompile(`\[([^\]]*)\]`)
)

// parseFilterComplex reads Plex's -filter_complex: labelled linear chains.
func parseFilterComplex(v string) ([]chain, error) {
	var out []chain
	for _, part := range splitUnescaped(v, ';') {
		m := chainShape.FindStringSubmatch(part)
		if m == nil {
			return nil, refuse("filter chain %q", part)
		}
		c := chain{out: m[3]}
		for _, l := range label.FindAllStringSubmatch(m[1], -1) {
			c.in = append(c.in, l[1])
		}
		for _, f := range splitUnescaped(m[2], ',') {
			name, args, _ := strings.Cut(strings.TrimSpace(f), "=")
			if name == "" {
				return nil, refuse("filter chain %q", part)
			}
			fl := filter{name: name}
			if args != "" {
				for _, a := range splitUnescaped(args, ':') {
					k, val, ok := strings.Cut(a, "=")
					if !ok {
						k, val = "", k
					}
					fl.args = append(fl.args, kv{k: k, v: strings.Trim(val, "'")})
				}
			}
			c.filters = append(c.filters, fl)
		}
		out = append(out, c)
	}
	return out, nil
}

// splitUnescaped splits s at sep, except where a backslash escapes it.
func splitUnescaped(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case sep:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// overlay is what a two-input overlay chain draws on the video: an input
// stream through its own chain (finding B's image subtitle).
type overlay struct {
	src     int
	filters []filter
}

// resolve follows a -map back to the input stream it is made from: "0:N"
// directly, or a chain's output label through every chain feeding it.
// Single-input chains are followed, and one overlay chain: its first input
// is the video followed on, its second what is drawn over it.
func resolve(m string, chains []chain) (int, []filter, *overlay, error) {
	if s := streamRef.FindStringSubmatch(m); s != nil {
		n, _ := strconv.Atoi(s[1])
		return n, nil, nil, nil
	}
	l := label.FindStringSubmatch(m)
	if l == nil || l[0] != m {
		return 0, nil, nil, refuse("map %q", m)
	}
	return follow(l[1], chains, true)
}

func follow(want string, chains []chain, overlays bool) (int, []filter, *overlay, error) {
	var (
		filters []filter
		over    *overlay
	)
	for range len(chains) + 1 {
		var c *chain
		for i := range chains {
			if chains[i].out == want {
				c = &chains[i]
				break
			}
		}
		if c == nil {
			return 0, nil, nil, refuse("label %q names no chain", want)
		}
		switch {
		case len(c.in) == 2 && len(c.filters) == 1 && c.filters[0].name == "overlay" && len(c.filters[0].args) == 0:
			if !overlays || over != nil {
				return 0, nil, nil, refuse("a second overlay")
			}
			src, fs, _, err := follow2(c.in[1], chains)
			if err != nil {
				return 0, nil, nil, err
			}
			over = &overlay{src: src, filters: fs}
		case len(c.in) != 1:
			return 0, nil, nil, refuse("a chain of %d inputs", len(c.in))
		default:
			filters = append(append([]filter(nil), c.filters...), filters...)
		}
		if s := streamRef.FindStringSubmatch(c.in[0]); s != nil {
			n, _ := strconv.Atoi(s[1])
			return n, filters, over, nil
		}
		want = c.in[0]
	}
	return 0, nil, nil, refuse("label %q loops", want)
}

// follow2 resolves an overlay's second input: a stream, directly or
// through single-input chains.
func follow2(in string, chains []chain) (int, []filter, *overlay, error) {
	if s := streamRef.FindStringSubmatch(in); s != nil {
		n, _ := strconv.Atoi(s[1])
		return n, nil, nil, nil
	}
	return follow(in, chains, false)
}
