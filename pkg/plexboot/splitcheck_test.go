package plexboot

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheRealDumpKeepsEveryColumnOfPluginPrefixes guards the splitter against
// the actual dump rather than a hand-written sample.
//
// plugin_prefixes is the table that caught this: the live database ended up
// with its first eight columns and not the last two, so Plex's query for
// plugin_prefixes.prefs found no such column and the shim walked off the end
// of the result and segfaulted. That is upstream's issue #26.
func TestTheRealDumpKeepsEveryColumnOfPluginPrefixes(t *testing.T) {
	body, err := os.ReadFile(schemaDir + "/plex_schema.sql")
	require.NoError(t, err)

	var found string
	for _, seg := range Segments(string(body)) {
		if strings.Contains(seg.SQL, "CREATE TABLE plex.plugin_prefixes") {
			found = seg.SQL
		}
	}
	require.NotEmpty(t, found, "the plugin_prefixes statement went missing entirely")
	assert.Contains(t, found, "has_store_services")
	assert.Contains(t, found, "prefs")
}
