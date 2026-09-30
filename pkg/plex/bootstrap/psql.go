package bootstrap

import (
	"fmt"
	"regexp"
	"strings"
)

// Command is one unit of a psql script: a statement, a psql meta-command, or a
// COPY statement together with the rows that follow it.
type Command struct {
	// Line is where the command starts, 1-based, for error messages.
	Line int
	// SQL is the statement without its terminating semicolon. Empty for a
	// meta-command.
	SQL string
	// Meta is a backslash command such as pg_dump's `\restrict <key>`. psql
	// runs these itself; nothing is sent to the server.
	Meta string
	// Copy holds the rows of a `COPY ... FROM stdin` statement, one per line
	// with the terminating `\.` removed. Nil for anything else.
	Copy []byte
}

// copyFromStdin matches a COPY whose data follows inline in the script.
var copyFromStdin = regexp.MustCompile(`(?is)^\s*COPY\s.*\sFROM\s+stdin\b`)

// dollarTag matches the opening of a dollar-quoted string, `$$` or `$tag$`.
var dollarTag = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)?\$`)

// ParsePSQL splits a script written for psql -- a pg_dump, or statements
// written by hand -- into the commands psql would run, in order.
//
// It reads what psql itself would: semicolons end statements except inside
// quotes, dollar quotes and comments; a backslash at the start of a line
// outside a statement is a meta-command; and a COPY FROM stdin is followed by
// its rows up to a line holding `\.`. A statement left open at the end runs
// too, as psql runs it at end of file.
func ParsePSQL(src string) ([]Command, error) {
	p := &psqlParser{src: src, line: 1}
	return p.parse()
}

type psqlParser struct {
	src  string
	i    int
	line int

	cmds    []Command
	buf     strings.Builder
	content bool // the statement so far holds more than whitespace and comments
	start   int  // line on which the statement's content began
}

func (p *psqlParser) parse() ([]Command, error) {
	for p.i < len(p.src) {
		c := p.src[p.i]
		atLineStart := p.i == 0 || p.src[p.i-1] == '\n'

		switch {
		case atLineStart && !p.content && c == '\\':
			p.meta()
		case c == '-' && p.peek(1) == '-':
			p.lineComment()
		case c == '/' && p.peek(1) == '*':
			if err := p.blockComment(); err != nil {
				return nil, err
			}
		case c == '\'':
			if err := p.quoted('\'', p.escapeString()); err != nil {
				return nil, err
			}
		case c == '"':
			if err := p.quoted('"', false); err != nil {
				return nil, err
			}
		case c == '$' && p.dollarQuoteStart() != "":
			if err := p.dollarQuoted(); err != nil {
				return nil, err
			}
		case c == ';':
			p.i++
			if err := p.endStatement(); err != nil {
				return nil, err
			}
		default:
			p.take(1)
		}
	}
	if p.content {
		p.emit()
	}
	return p.cmds, nil
}

func (p *psqlParser) peek(n int) byte {
	if p.i+n < len(p.src) {
		return p.src[p.i+n]
	}
	return 0
}

// take appends n bytes to the statement, counting lines and marking content.
func (p *psqlParser) take(n int) {
	chunk := p.src[p.i : p.i+n]
	p.markContent(chunk)
	p.buf.WriteString(chunk)
	p.line += strings.Count(chunk, "\n")
	p.i += n
}

func (p *psqlParser) markContent(chunk string) {
	if p.content || strings.TrimSpace(chunk) == "" {
		return
	}
	p.content = true
	// A statement starts on the line of its first non-space byte.
	p.start = p.line + strings.Count(chunk[:len(chunk)-len(strings.TrimLeft(chunk, " \t\r\n"))], "\n")
}

// meta records a backslash command, which runs to the end of its line.
func (p *psqlParser) meta() {
	end := strings.IndexByte(p.src[p.i:], '\n')
	if end < 0 {
		end = len(p.src) - p.i
	}
	cmd := strings.TrimRight(p.src[p.i:p.i+end], "\r")
	p.cmds = append(p.cmds, Command{Line: p.line, Meta: cmd})
	// Comments gathered before it belong to nothing.
	p.buf.Reset()
	p.i += end
}

// lineComment skips `--` to the end of the line. Comments before a statement
// are dropped so that the statement begins with its keyword; comments inside
// one are kept, since the server accepts them.
func (p *psqlParser) lineComment() {
	end := strings.IndexByte(p.src[p.i:], '\n')
	if end < 0 {
		end = len(p.src) - p.i
	}
	if p.content {
		p.buf.WriteString(p.src[p.i : p.i+end])
	}
	p.i += end
}

// blockComment skips a /* */ comment, which PostgreSQL lets nest.
func (p *psqlParser) blockComment() error {
	startLine, depth, j := p.line, 0, p.i
	for j < len(p.src) {
		switch {
		case strings.HasPrefix(p.src[j:], "/*"):
			depth++
			j += 2
		case strings.HasPrefix(p.src[j:], "*/"):
			depth--
			j += 2
			if depth == 0 {
				chunk := p.src[p.i:j]
				if p.content {
					p.buf.WriteString(chunk)
				}
				p.line += strings.Count(chunk, "\n")
				p.i = j
				return nil
			}
		default:
			j++
		}
	}
	return fmt.Errorf("line %d: unterminated block comment", startLine)
}

// escapeString reports whether the quote at p.i opens an E” string, in
// which a backslash escapes the next character.
func (p *psqlParser) escapeString() bool {
	if p.i == 0 {
		return false
	}
	prev := p.src[p.i-1]
	if prev != 'E' && prev != 'e' {
		return false
	}
	return p.i < 2 || !isIdentByte(p.src[p.i-2])
}

// quoted takes a '...' or "..." run, where a doubled quote is a literal one.
func (p *psqlParser) quoted(q byte, backslash bool) error {
	startLine, j := p.line, p.i+1
	for j < len(p.src) {
		switch c := p.src[j]; {
		case backslash && c == '\\':
			j += 2
		case c == q && j+1 < len(p.src) && p.src[j+1] == q:
			j += 2
		case c == q:
			p.take(j + 1 - p.i)
			return nil
		default:
			j++
		}
	}
	return fmt.Errorf("line %d: unterminated %c quote", startLine, q)
}

// dollarQuoteStart returns the opening delimiter at p.i, or "" when the `$`
// is not one: a positional parameter ($1) or part of an identifier (a$b).
func (p *psqlParser) dollarQuoteStart() string {
	if p.i > 0 && isIdentByte(p.src[p.i-1]) {
		return ""
	}
	return dollarTag.FindString(p.src[p.i:])
}

func (p *psqlParser) dollarQuoted() error {
	tag, startLine := p.dollarQuoteStart(), p.line
	end := strings.Index(p.src[p.i+len(tag):], tag)
	if end < 0 {
		return fmt.Errorf("line %d: unterminated dollar quote %s", startLine, tag)
	}
	p.take(len(tag) + end + len(tag))
	return nil
}

// endStatement closes the current statement at a semicolon. For a COPY FROM
// stdin it also consumes the rows that follow, up to `\.`.
func (p *psqlParser) endStatement() error {
	if !p.content {
		p.buf.Reset()
		return nil
	}
	sql := p.emit()
	if !copyFromStdin.MatchString(sql) {
		return nil
	}

	// The rows start on the line after the statement.
	if nl := strings.IndexByte(p.src[p.i:], '\n'); nl >= 0 {
		p.i += nl + 1
		p.line++
	} else {
		p.i = len(p.src)
	}
	copyLine := p.cmds[len(p.cmds)-1].Line
	var rows strings.Builder
	for p.i < len(p.src) {
		end := strings.IndexByte(p.src[p.i:], '\n')
		next := p.i + end + 1
		if end < 0 {
			end, next = len(p.src)-p.i, len(p.src)
		}
		row := strings.TrimRight(p.src[p.i:p.i+end], "\r")
		p.i = next
		p.line++
		if row == `\.` {
			p.cmds[len(p.cmds)-1].Copy = []byte(rows.String())
			return nil
		}
		rows.WriteString(row)
		rows.WriteByte('\n')
	}
	return fmt.Errorf("line %d: COPY data has no terminating \\. line", copyLine)
}

// emit appends the statement gathered so far and resets for the next one.
func (p *psqlParser) emit() string {
	sql := strings.TrimSpace(p.buf.String())
	p.cmds = append(p.cmds, Command{Line: p.start, SQL: sql})
	p.buf.Reset()
	p.content = false
	return sql
}

func isIdentByte(b byte) bool {
	return b == '_' || b == '$' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}
