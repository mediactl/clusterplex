package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// podIdentity sets the downward-API variables every test needs.
func podIdentity(t *testing.T) {
	t.Helper()
	t.Setenv("POD_NAME", "plex-0")
	t.Setenv("POD_NAMESPACE", "media")
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	return p
}

func TestConfigDefaults(t *testing.T) {
	podIdentity(t)
	c, err := loadConfig(nil)
	require.NoError(t, err)

	assert.Equal(t, "/usr/lib/plexmediaserver/Plex Media Server", c.PMSBinary)
	assert.Equal(t, "/usr/lib/plexmediaserver", c.BinDir)
	assert.Equal(t, 32400, c.PMSPort)
	assert.Equal(t, 32499, c.ProxyPort)
	assert.Equal(t, 50051, c.WorkerPort)
	assert.Equal(t, 20202, c.LiteFSPort)
	assert.Equal(t, "/var/run/clusterplex.sock", c.Socket)
	assert.Equal(t, "/var/lib/litefs", c.LiteFSDir)
	assert.Equal(t, "/var/lib/plexmediaserver/Library/Application Support/Plex Media Server", c.PlexDir)
	assert.Equal(t, "/var/lib/plexmediaserver/Library/Application Support/Plex Media Server/plexmediaserver.pid", c.PIDFile())
	assert.Equal(t, "cluster-plex-litefs", c.LeaseName)
	assert.Equal(t, "plex-workers", c.WorkersService)
	assert.Equal(t, "plex-0.plex-workers.media.svc.cluster.local:32400", c.PMSAddr())
	assert.Equal(t, "http://plex-0.plex-workers.media.svc.cluster.local:20202", c.AdvertiseURL())
	assert.Empty(t, c.Preferences)
}

func TestConfigKeepsWorkingWithTheExistingEnvironmentVariableNames(t *testing.T) {
	podIdentity(t)
	t.Setenv("CLUSTERPLEX_PROXY_PORT", "1234")
	t.Setenv("CLUSTERPLEX_PMS_BINARY", "/opt/pms")

	c, err := loadConfig(nil)
	require.NoError(t, err)
	assert.Equal(t, 1234, c.ProxyPort)
	assert.Equal(t, "/opt/pms", c.PMSBinary)
}

func TestConfigReadsAYAMLFile(t *testing.T) {
	podIdentity(t)
	path := writeConfig(t, `
proxy-port: 4321
plex-dir: /srv/plex
plex:
  preferences:
    - name: FriendlyName
      value: Cluster Plex
    - name: LogVerbose
      value: "1"
`)
	c, err := loadConfig([]string{"--config", path})
	require.NoError(t, err)

	assert.Equal(t, 4321, c.ProxyPort)
	assert.Equal(t, "/srv/plex", c.PlexDir)
	assert.Equal(t, map[string]string{"FriendlyName": "Cluster Plex", "LogVerbose": "1"}, c.Preferences)
}

func TestConfigPrecedenceIsFlagThenEnvThenFile(t *testing.T) {
	podIdentity(t)
	path := writeConfig(t, "proxy-port: 1111\nworker-port: 2222\nlitefs-port: 3333\n")
	t.Setenv("CLUSTERPLEX_PROXY_PORT", "4444")
	t.Setenv("CLUSTERPLEX_WORKER_PORT", "5555")

	c, err := loadConfig([]string{"--config", path, "--proxy-port", "6666"})
	require.NoError(t, err)

	assert.Equal(t, 6666, c.ProxyPort, "flag wins")
	assert.Equal(t, 5555, c.WorkerPort, "env beats file")
	assert.Equal(t, 3333, c.LiteFSPort, "file beats default")
}

func TestPreferencesComeFromFileEnvAndFlags(t *testing.T) {
	podIdentity(t)
	path := writeConfig(t, `
plex:
  preferences:
    - name: FromFile
      value: file
`)
	t.Setenv("CLUSTERPLEX_PLEX_PREFERENCE_FromEnv", "env")

	c, err := loadConfig([]string{"--config", path, "--plex-preference", "FromFlag=flag"})
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"FromFile": "file", "FromEnv": "env", "FromFlag": "flag"}, c.Preferences)
}

func TestPreferenceEnvVariablesKeepTheirCaseBecausePlexKeysAreCaseSensitive(t *testing.T) {
	podIdentity(t)
	t.Setenv("CLUSTERPLEX_PLEX_PREFERENCE_FriendlyName", "Cluster Plex")

	c, err := loadConfig(nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"FriendlyName": "Cluster Plex"}, c.Preferences)
}

func TestPreferencePrecedenceIsFlagThenEnvThenFile(t *testing.T) {
	podIdentity(t)
	path := writeConfig(t, `
plex:
  preferences:
    - name: FriendlyName
      value: from-file
    - name: LogVerbose
      value: from-file
`)
	t.Setenv("CLUSTERPLEX_PLEX_PREFERENCE_FriendlyName", "from-env")
	t.Setenv("CLUSTERPLEX_PLEX_PREFERENCE_LogVerbose", "from-env")

	c, err := loadConfig([]string{"--config", path, "--plex-preference", "FriendlyName=from-flag"})
	require.NoError(t, err)

	assert.Equal(t, "from-flag", c.Preferences["FriendlyName"])
	assert.Equal(t, "from-env", c.Preferences["LogVerbose"])
}

func TestPreferenceValuesMayContainEqualsSigns(t *testing.T) {
	podIdentity(t)
	c, err := loadConfig([]string{"--plex-preference", "customConnections=http://a/?x=1"})
	require.NoError(t, err)
	assert.Equal(t, "http://a/?x=1", c.Preferences["customConnections"])
}

func TestConfigRejectsAPreferenceThatWouldOverwriteTheServerIdentity(t *testing.T) {
	podIdentity(t)
	_, err := loadConfig([]string{"--plex-preference", "MachineIdentifier=nope"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MachineIdentifier")
}

func TestConfigRejectsAMalformedPreferenceFlag(t *testing.T) {
	podIdentity(t)
	_, err := loadConfig([]string{"--plex-preference", "NoEqualsSign"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NoEqualsSign")
}

func TestConfigRejectsAMissingConfigFile(t *testing.T) {
	podIdentity(t)
	_, err := loadConfig([]string{"--config", "/nonexistent/config.yaml"})
	require.Error(t, err)
}

func TestConfigRejectsMalformedPort(t *testing.T) {
	podIdentity(t)
	t.Setenv("CLUSTERPLEX_PROXY_PORT", "abc")
	_, err := loadConfig(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "proxy-port")
}

func TestConfigRequiresPodIdentity(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	_, err := loadConfig(nil)
	require.Error(t, err)
}

func TestMachineIdentifierSettingPinsTheServerIdentity(t *testing.T) {
	podIdentity(t)
	const id = "9c67996e-8b08-44b9-9c83-a6d317322a2d"

	c, err := loadConfig([]string{"--plex-machine-identifier", id})
	require.NoError(t, err)
	assert.Equal(t, id, c.Preferences["MachineIdentifier"])
}

func TestMachineIdentifierSettingReadsTheConfigFileAndEnvironment(t *testing.T) {
	podIdentity(t)
	path := writeConfig(t, "plex:\n  machine-identifier: 9c67996e-8b08-44b9-9c83-a6d317322a2d\n")

	fromFile, err := loadConfig([]string{"--config", path})
	require.NoError(t, err)
	assert.Equal(t, "9c67996e-8b08-44b9-9c83-a6d317322a2d", fromFile.Preferences["MachineIdentifier"])

	t.Setenv("CLUSTERPLEX_PLEX_MACHINE_IDENTIFIER", "99999999-8888-7777-6666-555555555555")
	fromEnv, err := loadConfig([]string{"--config", path})
	require.NoError(t, err)
	assert.Equal(t, "99999999-8888-7777-6666-555555555555", fromEnv.Preferences["MachineIdentifier"], "env beats file")
}

func TestMachineIdentifierMustBeAUUID(t *testing.T) {
	podIdentity(t)
	_, err := loadConfig([]string{"--plex-machine-identifier", "not-a-uuid"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UUID")
}

func TestMachineIdentifierConflictingWithAnExplicitPreferenceIsRejected(t *testing.T) {
	podIdentity(t)
	_, err := loadConfig([]string{
		"--plex-machine-identifier", "9c67996e-8b08-44b9-9c83-a6d317322a2d",
		"--plex-preference", "MachineIdentifier=99999999-8888-7777-6666-555555555555",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "machine-identifier")
}

func TestMachineIdentifierAgreeingWithAnExplicitPreferenceIsFine(t *testing.T) {
	podIdentity(t)
	const id = "9c67996e-8b08-44b9-9c83-a6d317322a2d"
	c, err := loadConfig([]string{"--plex-machine-identifier", id, "--plex-preference", "MachineIdentifier=" + id})
	require.NoError(t, err)
	assert.Equal(t, id, c.Preferences["MachineIdentifier"])
}

func TestAdoptClusterIDIsOffByDefaultBecauseItDiscardsData(t *testing.T) {
	podIdentity(t)
	c, err := loadConfig(nil)
	require.NoError(t, err)
	assert.False(t, c.AdoptClusterID)

	c, err = loadConfig([]string{"--litefs-adopt-cluster-id"})
	require.NoError(t, err)
	assert.True(t, c.AdoptClusterID)
}
