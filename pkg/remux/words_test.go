package remux

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// words splits a command line as Plex logs it: single quotes literal,
// double quotes with backslash escapes, a backslash escaping one byte.
func words(line string) []string {
	var out []string
	var cur strings.Builder
	in, quote := false, byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			switch {
			case c == '\\' && i+1 < len(line):
				i++
				cur.WriteByte(line[i])
			case c == '"':
				quote = 0
			default:
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
			in = true
		case c == ' ':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// loggedJob is one line of plex-dash-jobs.log as the shim would send it:
// the leading VAR=value words as the environment, the binary dropped.
func loggedJob(line string) (args []string, env map[string]string) {
	env = map[string]string{}
	w := words(line)
	for len(w) > 0 && strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "-") && !strings.HasPrefix(w[0], "/") {
		k, v, _ := strings.Cut(w[0], "=")
		env[k] = v
		w = w[1:]
	}
	return w[1:], env // w[0] is "/usr/lib/plexmediaserver/Plex Transcoder"
}

func loggedJobs(t *testing.T) [][]string {
	t.Helper()
	f, err := os.Open("testdata/plex-dash-jobs.log")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out [][]string
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		args, _ := loggedJob(s.Text())
		out = append(out, args)
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) != 44 {
		t.Fatalf("fixture has %d jobs, want 44", len(out))
	}
	return out
}
