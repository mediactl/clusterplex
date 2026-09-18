package plexboot

import "strings"

// Segment is one piece of a dump: either SQL to execute, or a COPY command
// with the rows that belong to it.
type Segment struct {
	// SQL is the statement text, or the COPY command when Data is set.
	SQL string
	// Data is the rows to stream, empty for ordinary SQL.
	Data string
}

// IsCopy reports whether this segment streams rows rather than executing SQL.
func (s Segment) IsCopy() bool { return s.Data != "" }

// Segments splits a pg_dump into pieces that can be sent to the server.
//
// Two things in a dump are not SQL. psql's own backslash commands, such as
// \restrict, which the server rejects. And the rows inside a COPY block, which
// are tab-separated values terminated by a line holding only "\." — sending
// those as SQL fails on the first row that does not parse as a statement,
// leaving a schema that is half built and looks finished.
//
// Inside a COPY block a leading backslash is data — "\N" is how a NULL is
// written — so meta-commands are only recognised outside one.
func Segments(dump string) []Segment {
	var (
		out     []Segment
		sql     strings.Builder
		data    strings.Builder
		inCopy  bool
		flushed = func(b *strings.Builder) string {
			s := b.String()
			b.Reset()
			return s
		}
	)

	for line := range strings.SplitSeq(dump, "\n") {
		if inCopy {
			// Only this exact line ends the block; anything else is a row.
			if strings.TrimRight(line, "\r") == `\.` {
				out = append(out, Segment{SQL: flushed(&sql), Data: flushed(&data)})
				inCopy = false
				continue
			}
			data.WriteString(line)
			data.WriteString("\n")
			continue
		}

		if strings.HasPrefix(line, `\`) {
			continue // a psql meta-command
		}

		if isCopyFromStdin(line) {
			// Everything accumulated so far runs before the copy does.
			out = append(out, statements(flushed(&sql))...)
			sql.Reset()
			sql.WriteString(strings.TrimSpace(line))
			inCopy = true
			continue
		}

		sql.WriteString(line)
		sql.WriteString("\n")
	}

	// An unterminated COPY block is a truncated dump; keep what was read so
	// the failure comes from the server rather than from silently dropping it.
	if inCopy {
		out = append(out, Segment{SQL: flushed(&sql), Data: flushed(&data)})
	} else {
		out = append(out, statements(flushed(&sql))...)
	}
	return out
}

// statements turns a run of SQL into one segment per statement. They go to the
// server individually because a batch shares an implicit transaction, and this
// dump contains statements that are expected to fail.
func statements(sql string) []Segment {
	parts := splitStatements(sql)
	out := make([]Segment, 0, len(parts))
	for _, p := range parts {
		out = append(out, Segment{SQL: p})
	}
	return out
}

// isCopyFromStdin reports whether a line opens a COPY block.
func isCopyFromStdin(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(strings.ToUpper(trimmed), "COPY ") {
		return false
	}
	upper := strings.ToUpper(trimmed)
	return strings.HasSuffix(upper, "FROM STDIN;") || strings.Contains(upper, "FROM STDIN ")
}
