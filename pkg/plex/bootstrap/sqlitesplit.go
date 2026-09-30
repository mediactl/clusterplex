package bootstrap

import (
	"fmt"
	"regexp"
	"strings"
)

// Statement is one SQLite statement from a script, with the line it starts on.
type Statement struct {
	Line int
	SQL  string
}

var (
	createTrigger = regexp.MustCompile(`(?i)^\s*CREATE\s+(TEMP\s+|TEMPORARY\s+)?TRIGGER\b`)
	triggerEnd    = regexp.MustCompile(`(?i)\bEND\s*$`)
)

// SplitSQLite splits a script into statements as the sqlite3 command reads
// it: a semicolon ends a statement outside quotes and comments, except inside
// a CREATE TRIGGER body, which runs to the semicolon after its END.
func SplitSQLite(src string) ([]Statement, error) {
	var (
		out     []Statement
		buf     strings.Builder
		line    = 1
		start   = 0
		content = false
	)
	flush := func() {
		if content {
			out = append(out, Statement{Line: start, SQL: strings.TrimSpace(buf.String())})
		}
		buf.Reset()
		content = false
	}
	take := func(s string) {
		if !content && strings.TrimSpace(s) != "" {
			content = true
			start = line + strings.Count(s[:len(s)-len(strings.TrimLeft(s, " \t\r\n"))], "\n")
		}
		buf.WriteString(s)
		line += strings.Count(s, "\n")
	}

	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src) - i
			}
			i += end
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("line %d: unterminated block comment", line)
			}
			line += strings.Count(src[i:i+2+end+2], "\n")
			i += 2 + end + 2
		case c == '\'' || c == '"' || c == '`' || c == '[':
			closer := c
			if c == '[' {
				closer = ']'
			}
			j := i + 1
			for ; j < len(src); j++ {
				if src[j] != closer {
					continue
				}
				// A doubled quote is a literal one; brackets have no escape.
				if closer != ']' && j+1 < len(src) && src[j+1] == closer {
					j++
					continue
				}
				break
			}
			if j >= len(src) {
				return nil, fmt.Errorf("line %d: unterminated %c quote", line, c)
			}
			take(src[i : j+1])
			i = j + 1
		case c == ';':
			stmt := buf.String()
			if createTrigger.MatchString(stmt) && !triggerEnd.MatchString(strings.TrimSpace(stmt)) {
				take(";")
				i++
				continue
			}
			i++
			flush()
		default:
			take(src[i : i+1])
			i++
		}
	}
	flush()
	return out, nil
}
