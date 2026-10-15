package plexscan

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The captured invocation (testdata/argv.txt, from talos on 2026-10-15) is
// the only way Plex 1.43 runs the binary: an analyze of one item.
func TestParseAnalyzeReadsTheCapturedInvocation(t *testing.T) {
	b, err := os.ReadFile("testdata/argv.txt")
	require.NoError(t, err)
	seen := 0
	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		_, rest, _ := strings.Cut(line, "Plex Media Scanner.real")
		item, ok := ParseAnalyze(strings.Fields(rest))
		require.True(t, ok, line)
		require.Equal(t, int64(14371), item)
		seen++
	}
	require.Equal(t, 1, seen)
}

// A scan, a tree, an analyze with no or a bad item: not ours.
func TestParseAnalyzeLeavesScansAndTreesAlone(t *testing.T) {
	for _, args := range [][]string{
		{"--scan", "--refresh", "--section", "2"}, {"--analyze"}, {"--analyze", "--item", "x"}, {"--tree", "--item", "3"}, {},
	} {
		_, ok := ParseAnalyze(args)
		require.False(t, ok, "%v", args)
	}
}
