package plexboot

import "strings"

// splitStatements breaks SQL into individual statements.
//
// They have to go to the server one at a time. A batch sent as one string runs
// in an implicit transaction, so a single failing statement rolls back
// everything sent with it — and this dump contains statements that are
// expected to fail, which would take CREATE SCHEMA down with them.
//
// Splitting means knowing where a semicolon is not a boundary: inside a string
// literal, inside a dollar-quoted body (every function in the compatibility
// file is one), or inside a comment.
func splitStatements(sql string) []string {
	var (
		out   []string
		cur   strings.Builder
		i     int
		runes = []rune(sql)
	)

	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}

	for i < len(runes) {
		switch {
		case runes[i] == '\'':
			i = copyQuoted(runes, i, &cur)

		case runes[i] == '-' && next(runes, i) == '-':
			i = copyUntil(runes, i, "\n", &cur)

		case runes[i] == '/' && next(runes, i) == '*':
			i = copyUntil(runes, i, "*/", &cur)

		case runes[i] == '$':
			if tag, ok := dollarTag(runes, i); ok {
				i = copyDollarQuoted(runes, i, tag, &cur)
				continue
			}
			cur.WriteRune(runes[i])
			i++

		case runes[i] == ';':
			flush()
			i++

		default:
			cur.WriteRune(runes[i])
			i++
		}
	}
	flush()
	return out
}

// next returns the rune after i, or 0 at the end.
func next(runes []rune, i int) rune {
	if i+1 < len(runes) {
		return runes[i+1]
	}
	return 0
}

// copyQuoted copies a single-quoted literal, where ” is an escaped quote.
func copyQuoted(runes []rune, i int, cur *strings.Builder) int {
	cur.WriteRune(runes[i])
	i++
	for i < len(runes) {
		if runes[i] == '\'' {
			cur.WriteRune(runes[i])
			i++
			// A doubled quote is one quote of data, not the end.
			if i < len(runes) && runes[i] == '\'' {
				cur.WriteRune(runes[i])
				i++
				continue
			}
			return i
		}
		cur.WriteRune(runes[i])
		i++
	}
	return i
}

// copyUntil copies through the end of a comment, terminator included.
func copyUntil(runes []rune, i int, end string, cur *strings.Builder) int {
	term := []rune(end)
	for i < len(runes) {
		if matchesAt(runes, i, term) {
			for _, r := range term {
				cur.WriteRune(r)
			}
			return i + len(term)
		}
		cur.WriteRune(runes[i])
		i++
	}
	return i
}

// dollarTag reads a dollar quote opener such as $$ or $body$ at i.
func dollarTag(runes []rune, i int) (string, bool) {
	j := i + 1
	for j < len(runes) && runes[j] != '$' {
		// A tag is an identifier; anything else means this is not a quote.
		if !isTagRune(runes[j]) {
			return "", false
		}
		j++
	}
	if j >= len(runes) {
		return "", false
	}
	return string(runes[i : j+1]), true
}

func isTagRune(r rune) bool {
	return r == '_' ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

// copyDollarQuoted copies a dollar-quoted body, closing tag included.
func copyDollarQuoted(runes []rune, i int, tag string, cur *strings.Builder) int {
	term := []rune(tag)
	for _, r := range term {
		cur.WriteRune(r)
	}
	i += len(term)
	for i < len(runes) {
		if matchesAt(runes, i, term) {
			for _, r := range term {
				cur.WriteRune(r)
			}
			return i + len(term)
		}
		cur.WriteRune(runes[i])
		i++
	}
	return i
}

func matchesAt(runes []rune, i int, want []rune) bool {
	if i+len(want) > len(runes) {
		return false
	}
	for k, r := range want {
		if runes[i+k] != r {
			return false
		}
	}
	return true
}
