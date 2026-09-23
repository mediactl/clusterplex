package plexprefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// live is a Preferences.xml as Plex itself writes one, including the identity
// attributes the manager must never touch.
const live = `<?xml version="1.0" encoding="utf-8"?>
<Preferences IPNetworkType="dualstack" OldestPreviousVersion="1.43.4.10903-e5521bd8c" MachineIdentifier="9c67996e-8b08-44b9-9c83-a6d317322a2d" ProcessedMachineIdentifier="25648e79229b88b46fdb829e0bdabedf7c385304" AnonymousMachineIdentifier="06cf32a6-828a-46d4-abaa-1717e2f90034" PlexOnlineToken="secret-token" _10de1f141a58200c00000100.0-TranscodeCountLimit="0"/>
`

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "Preferences.xml")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

// attrs parses a written file back into a name/value map.
func attrs(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	got, err := parse(b)
	require.NoError(t, err)
	out := map[string]string{}
	for _, a := range got {
		out[a.Name] = a.Value
	}
	return out
}

func TestApplyPreservesAttributesItDoesNotManage(t *testing.T) {
	path := writeFile(t, live)

	changed, err := Apply(path, map[string]string{"FriendlyName": "Cluster Plex"})
	require.NoError(t, err)
	assert.Equal(t, []string{"FriendlyName"}, changed)

	got := attrs(t, path)
	assert.Equal(t, "Cluster Plex", got["FriendlyName"])
	assert.Equal(t, "9c67996e-8b08-44b9-9c83-a6d317322a2d", got["MachineIdentifier"], "server identity must survive")
	assert.Equal(t, "secret-token", got["PlexOnlineToken"], "the plex.tv token must survive")
	assert.Equal(t, "0", got["_10de1f141a58200c00000100.0-TranscodeCountLimit"])
	assert.Equal(t, "dualstack", got["IPNetworkType"])
}

func TestApplyOverwritesDeclaredKeys(t *testing.T) {
	path := writeFile(t, live)

	changed, err := Apply(path, map[string]string{"IPNetworkType": "ipv4"})
	require.NoError(t, err)

	assert.Equal(t, []string{"IPNetworkType"}, changed)
	assert.Equal(t, "ipv4", attrs(t, path)["IPNetworkType"])
}

func TestApplyCreatesFileWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Preferences.xml")

	changed, err := Apply(path, map[string]string{"FriendlyName": "Fresh"})
	require.NoError(t, err)

	assert.Equal(t, []string{"FriendlyName"}, changed)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(b), `<?xml version="1.0" encoding="utf-8"?>`), "Plex expects the declaration")
	assert.Equal(t, "Fresh", attrs(t, path)["FriendlyName"])
}

func TestApplyLeavesAnAlreadyCorrectFileUntouched(t *testing.T) {
	path := writeFile(t, live)
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))

	changed, err := Apply(path, map[string]string{"IPNetworkType": "dualstack"})
	require.NoError(t, err)

	assert.Empty(t, changed)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.WithinDuration(t, old, info.ModTime(), time.Second, "an unchanged file must not be rewritten")
}

func TestApplyWritesOwnerOnlyBecauseTheFileHoldsTheToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Preferences.xml")
	_, err := Apply(path, map[string]string{"FriendlyName": "Fresh"})
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestApplyRefusesCorruptFileWithoutClobberingIt(t *testing.T) {
	path := writeFile(t, "<Preferences oops")

	_, err := Apply(path, map[string]string{"FriendlyName": "Cluster Plex"})
	require.Error(t, err)

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "<Preferences oops", string(b), "the original must be left for an operator to inspect")
}

func TestApplyEscapesValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Preferences.xml")
	_, err := Apply(path, map[string]string{"FriendlyName": `Ben & Jerry's "Server" <1>`})
	require.NoError(t, err)

	assert.Equal(t, `Ben & Jerry's "Server" <1>`, attrs(t, path)["FriendlyName"], "value must survive a write/read round trip")
}

func TestApplyKeepsExistingOrderAndAppendsNewKeysSorted(t *testing.T) {
	path := writeFile(t, `<?xml version="1.0" encoding="utf-8"?>
<Preferences BBB="2" AAA="1"/>
`)
	_, err := Apply(path, map[string]string{"ZZZ": "26", "MMM": "13", "AAA": "changed"})
	require.NoError(t, err)

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Regexp(t, `BBB="2" AAA="changed" MMM="13" ZZZ="26"`, string(b))
}

func TestApplyWithNoValuesIsANoOp(t *testing.T) {
	path := writeFile(t, live)
	changed, err := Apply(path, nil)
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestValidateAllowsPinningTheServerIdentity(t *testing.T) {
	// Pinning it is how every pod presents one server across a failover, and
	// how a rebuilt cluster keeps the identity clients already know.
	require.NoError(t, Validate(map[string]string{"MachineIdentifier": "9c67996e-8b08-44b9-9c83-a6d317322a2d"}))
	require.NoError(t, Validate(map[string]string{"AnonymousMachineIdentifier": "06cf32a6-828a-46d4-abaa-1717e2f90034"}))
}

func TestValidateRejectsTheDerivedIdentifierAndPointsAtTheRealSetting(t *testing.T) {
	// Plex computes it from MachineIdentifier with a salt we cannot reproduce,
	// so setting it by hand can only produce an inconsistent pair.
	err := Validate(map[string]string{"ProcessedMachineIdentifier": "deadbeef"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MachineIdentifier", "the error must name the setting to use instead")
}

func TestValidateRejectsAMachineIdentifierThatIsNotAUUID(t *testing.T) {
	for _, bad := range []string{"not-a-uuid", "9c67996e8b0844b99c83a6d317322a2d", ""} {
		require.Error(t, Validate(map[string]string{"MachineIdentifier": bad}), "value %q", bad)
	}
}

func TestValidateRejectsNamesThatAreNotLegalXMLAttributes(t *testing.T) {
	for name, key := range map[string]string{
		"empty":      "",
		"space":      "Friendly Name",
		"angle":      "Friendly<Name",
		"quote":      `Friendly"Name`,
		"leadDigit":  "1Friendly",
		"withColon":  "ns:Friendly",
		"withEquals": "Friendly=Name",
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, Validate(map[string]string{key: "x"}))
		})
	}
}

func TestValidateAcceptsRealPlexKeys(t *testing.T) {
	require.NoError(t, Validate(map[string]string{
		"FriendlyName": "Cluster Plex",
		"LogVerbose":   "1",
		"_10de1f141a58200c00000100.0-TranscodeCountLimit": "0",
		// Settable, though it caps one Plex process rather than the cluster:
		// see the note in docs/configuration.md.
		"WanTotalMaxUploadRate": "2000000",
	}))
}

func TestApplyClearsTheDerivedIdentifierWhenTheServerIdentityChanges(t *testing.T) {
	// Plex never recomputes ProcessedMachineIdentifier on its own. Leaving the
	// old one behind would keep clients seeing the previous server ID even
	// though the UUID changed, so it has to be removed for Plex to redo it.
	path := writeFile(t, live)

	changed, err := Apply(path, map[string]string{"MachineIdentifier": "99999999-8888-7777-6666-555555555555"})
	require.NoError(t, err)

	got := attrs(t, path)
	assert.Equal(t, "99999999-8888-7777-6666-555555555555", got["MachineIdentifier"])
	assert.NotContains(t, got, "ProcessedMachineIdentifier", "the stale derived value must be gone")
	assert.Contains(t, changed, "ProcessedMachineIdentifier", "removing it is a change worth reporting")
	assert.Equal(t, "secret-token", got["PlexOnlineToken"], "unrelated settings still survive")
}

func TestApplyKeepsTheDerivedIdentifierWhenTheServerIdentityIsUnchanged(t *testing.T) {
	path := writeFile(t, live)

	changed, err := Apply(path, map[string]string{"MachineIdentifier": "9c67996e-8b08-44b9-9c83-a6d317322a2d"})
	require.NoError(t, err)

	assert.Empty(t, changed, "declaring the identity Plex already has is not a change")
	assert.Equal(t, "25648e79229b88b46fdb829e0bdabedf7c385304", attrs(t, path)["ProcessedMachineIdentifier"])
}

func TestApplyLeavesTheDerivedIdentifierAloneWhenTheIdentityIsNotDeclared(t *testing.T) {
	path := writeFile(t, live)

	_, err := Apply(path, map[string]string{"FriendlyName": "Cluster Plex"})
	require.NoError(t, err)

	assert.Equal(t, "25648e79229b88b46fdb829e0bdabedf7c385304", attrs(t, path)["ProcessedMachineIdentifier"])
}

func TestApplySettingTheIdentityOnAFreshServerAddsNothingToRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Preferences.xml")

	changed, err := Apply(path, map[string]string{"MachineIdentifier": "99999999-8888-7777-6666-555555555555"})
	require.NoError(t, err)

	assert.Equal(t, []string{"MachineIdentifier"}, changed)
	assert.NotContains(t, attrs(t, path), "ProcessedMachineIdentifier")
}
