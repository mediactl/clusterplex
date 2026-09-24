package plexprefs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteAccessPublishingIsOff(t *testing.T) {
	// Every pod shares one server identity. If each published itself to
	// plex.tv the last one to check in would own the record, so clients would
	// be handed a pod address that only sometimes answers.
	assert.Equal(t, "0", Required()["PublishServerOnPlexOnlineKey"])
}

func TestAutomaticPortMappingIsOff(t *testing.T) {
	// UPnP and NAT-PMP would have Plex map a port on the router to a pod
	// address, bypassing the proxy and the load balancer both.
	assert.Equal(t, "1", Required()["ManualPortMappingMode"])
}

func TestCallersCannotMutateTheRequiredSettings(t *testing.T) {
	Required()["PublishServerOnPlexOnlineKey"] = "1"
	assert.Equal(t, "0", Required()["PublishServerOnPlexOnlineKey"])
}

func TestEverySettingTheArchitectureRequiresIsRejectedFromConfiguration(t *testing.T) {
	// The point of forcing them is that they cannot be argued with, so an
	// operator declaring one is a mistake worth failing on rather than a
	// setting to quietly overwrite.
	for name := range Required() {
		err := Validate(map[string]string{name: "1"})
		require.Error(t, err, "%s should be rejected", name)
		assert.Contains(t, err.Error(), name)
	}
}

func TestButlerTasksAreRejectedFromConfigurationToo(t *testing.T) {
	err := Validate(map[string]string{"ButlerTaskAnalyzeMedia": "1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ButlerTaskAnalyzeMedia")
}

func TestCustomConnectionsMustComeFromTheExternalURL(t *testing.T) {
	// It has to agree with what the proxy actually serves, and only the
	// external URL knows that.
	err := Validate(map[string]string{"customConnections": "https://plex.example.com"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "external-url")
}

func TestASettingTheArchitectureDoesNotClaimIsStillAllowed(t *testing.T) {
	assert.NoError(t, Validate(map[string]string{"FriendlyName": "Cluster Plex"}))
}

func TestTheExternalURLBecomesTheAdvertisedConnection(t *testing.T) {
	prefs := Enforced("https://plex.example.com:443", "/transcode")
	assert.Equal(t, "https://plex.example.com:443", prefs["customConnections"])
	assert.Equal(t, "0", prefs["PublishServerOnPlexOnlineKey"])
}

func TestNoExternalURLLeavesTheAdvertisedConnectionAlone(t *testing.T) {
	// Writing an empty customConnections would clear whatever Plex already
	// advertises, which is worse than not setting it.
	prefs := Enforced("", "")
	_, ok := prefs["customConnections"]
	assert.False(t, ok)
	assert.Equal(t, "1", prefs["ManualPortMappingMode"])
}

func TestLibraryScanSchedulingIsOff(t *testing.T) {
	// The Butler list covers analysis, thumbnails and metadata refresh, but
	// none of those discovers files. Scanning has its own two switches, and
	// with several pods watching one shared volume each would scan the same
	// tree into the same database at the same time.
	req := Required()
	assert.Equal(t, "0", req["FSEventLibraryUpdatesEnabled"])
	assert.Equal(t, "0", req["FSEventLibraryPartialScanEnabled"])
	assert.Equal(t, "0", req["ScheduledLibraryUpdatesEnabled"])
}

func TestScanSchedulingIsRejectedAndNamesWhatReplacesIt(t *testing.T) {
	// The replacement exists, so the message should say so rather than just
	// refusing: the refresh task is fanned out to one pod per library.
	err := Validate(map[string]string{"ScheduledLibraryUpdatesEnabled": "1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refresh")
}

func TestAllowedNetworksIsRefusedBecausePlexNeverSeesTheClientAddress(t *testing.T) {
	// It grants unauthenticated access by source address, and in this
	// architecture every request reaches Plex from the pod end of the veth.
	// So it is all-or-nothing: a range covering the link opens the server to
	// everyone who reaches the proxy, and any other range does nothing.
	err := Validate(map[string]string{"allowedNetworks": "10.0.0.0/8"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allowedNetworks")
}

func TestARefusedSettingIsNotWrittenToTheFile(t *testing.T) {
	// Refusing it is not the same as having an opinion about its value.
	// Writing an empty allowedNetworks would change Plex's behaviour in a way
	// nobody asked for.
	_, ok := Enforced("https://plex.example.com", "/transcode")["allowedNetworks"]
	assert.False(t, ok)
}
