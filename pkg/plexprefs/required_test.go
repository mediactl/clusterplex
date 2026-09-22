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
