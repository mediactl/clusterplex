package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plexprefs"
)

// podIdentity sets the minimum a manager needs to start: who this pod is, and
// where the shared library lives.
func podIdentity(t *testing.T) {
	t.Helper()
	t.Setenv("POD_NAME", "plex-0")
	t.Setenv("POD_NAMESPACE", "media")
	t.Setenv("CLUSTERPLEX_POSTGRES_HOST", "postgres")
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
	assert.Equal(t, "169.254.1.0/30", c.PlexSubnet.String())
	assert.Equal(t, 50051, c.WorkerPort)
	assert.Equal(t, "/var/run/clusterplex.sock", c.Socket)
	assert.Equal(t, "/var/lib/plexmediaserver/Library/Application Support/Plex Media Server", c.PlexDir)
	assert.Equal(t, "/var/lib/plexmediaserver/Library/Application Support/Plex Media Server/plexmediaserver.pid", c.PIDFile())
	assert.Equal(t, "cluster-plex-plextv", c.LeaseName)
	assert.Equal(t, "plex-workers", c.WorkersService)
	assert.Equal(t, "plex-0.plex-workers.media.svc.cluster.local:32400", c.PMSAddr())
	assert.Empty(t, c.Preferences)

	// Per-pod, because every pod runs Plex and a session's chunks are read
	// back by the pod that wrote them (ADR-0004).
	assert.Equal(t, "/transcode", c.TranscodeDir)

	assert.Equal(t, 5432, c.Postgres.Port)
	assert.Equal(t, "plex", c.Postgres.Database)
	assert.Equal(t, "plex", c.Postgres.User)
}

func TestConfigRequiresALibraryDatabase(t *testing.T) {
	// There is no local database any more, so a missing host is fatal rather
	// than something discovered on the first query.
	t.Setenv("POD_NAME", "plex-0")
	t.Setenv("POD_NAMESPACE", "media")
	t.Setenv("CLUSTERPLEX_POSTGRES_HOST", "")

	_, err := loadConfig(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host")
}

func TestPlexNeverRunsItsOwnScheduler(t *testing.T) {
	// Not a setting: every pod runs its own copy of the scheduler with no
	// knowledge of the others, so there is no mode in which leaving it on is
	// right. There is deliberately no flag to turn it back on.
	podIdentity(t)
	c, err := loadConfig([]string{"--postgres-host", "postgres"})
	require.NoError(t, err)

	for _, task := range plexprefs.ButlerTasks {
		assert.Equal(t, "0", c.EnforcedPreferences("")[task], task)
	}
}

func TestTheExternalURLIsWhatPlexAdvertises(t *testing.T) {
	podIdentity(t)
	c, err := loadConfig([]string{
		"--postgres-host", "postgres",
		"--plex-external-url", "https://plex.example.com:443",
	})
	require.NoError(t, err)
	assert.Equal(t, "https://plex.example.com:443", c.EnforcedPreferences(c.ExternalURL)["customConnections"])
}

func TestConfigRefusesAdvertisingAnAddressThatIsNotAURL(t *testing.T) {
	// It goes straight to clients, which simply fail to connect, so a typo
	// here is otherwise only visible as "remote access does not work".
	podIdentity(t)
	_, err := loadConfig([]string{"--postgres-host", "postgres", "--plex-external-url", "plex.example.com"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plex-external-url")
}

func TestUPnPAndRemoteAccessPublishingAreForcedOff(t *testing.T) {
	podIdentity(t)
	c, err := loadConfig([]string{"--postgres-host", "postgres"})
	require.NoError(t, err)

	prefs := c.EnforcedPreferences("")
	assert.Equal(t, "0", prefs["PublishServerOnPlexOnlineKey"])
	assert.Equal(t, "1", prefs["ManualPortMappingMode"])
	// GDM announces a pod's own address on the LAN. Every pod would announce
	// itself under one identity, and a client on the same network would reach
	// a pod directly rather than the proxy that pins its session.
	assert.Equal(t, "0", prefs["GdmEnabled"])
}

func TestTheTranscodeDirectoryIsForcedOntoPerPodStorage(t *testing.T) {
	// Every pod runs Plex (ADR-0004) and they share one volume for Metadata
	// and the identity. Transcode chunks must not go there: they are written
	// and read back by the one pod serving that session, which the proxy pins,
	// so shared storage would be a network round trip per chunk for nothing.
	// Left to itself Plex puts them under Cache, which is shared.
	podIdentity(t)
	c, err := loadConfig([]string{"--postgres-host", "postgres"})
	require.NoError(t, err)

	assert.Equal(t, "/transcode", c.EnforcedPreferences("")["TranscoderTempDirectory"])
}

func TestTheTranscodeDirectoryCanBeMovedButNotDeclared(t *testing.T) {
	// It has to match what the pod actually mounts, so it is a setting of its
	// own rather than a preference an operator writes by hand.
	podIdentity(t)
	c, err := loadConfig([]string{"--postgres-host", "postgres", "--plex-transcode-dir", "/fast/transcode"})
	require.NoError(t, err)
	assert.Equal(t, "/fast/transcode", c.EnforcedPreferences("")["TranscoderTempDirectory"])

	_, err = loadConfig([]string{"--plex-preference", "TranscoderTempDirectory=/tmp"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TranscoderTempDirectory")
}

func TestAForcedSettingCannotBeDeclaredAsAPreference(t *testing.T) {
	podIdentity(t)
	_, err := loadConfig([]string{"--plex-preference", "PublishServerOnPlexOnlineKey=1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PublishServerOnPlexOnlineKey")
}

func TestOperatorPreferencesCannotOverrideTheForcedOnes(t *testing.T) {
	// Belt and braces: even if one slipped past validation, the manager's own
	// values are applied last.
	podIdentity(t)
	c, err := loadConfig([]string{"--postgres-host", "postgres", "--plex-preference", "FriendlyName=Cluster Plex"})
	require.NoError(t, err)

	prefs := c.EnforcedPreferences("")
	assert.Equal(t, "Cluster Plex", prefs["FriendlyName"])
	assert.Equal(t, "0", prefs["ButlerTaskAnalyzeMedia"])
}

func TestConfigKeepsWorkingWithTheExistingEnvironmentVariableNames(t *testing.T) {
	podIdentity(t)
	t.Setenv("CLUSTERPLEX_WORKER_PORT", "1234")
	t.Setenv("CLUSTERPLEX_PMS_BINARY", "/opt/pms")

	c, err := loadConfig(nil)
	require.NoError(t, err)
	assert.Equal(t, 1234, c.WorkerPort)
	assert.Equal(t, "/opt/pms", c.PMSBinary)
}

func TestConfigReadsAYAMLFile(t *testing.T) {
	podIdentity(t)
	path := writeConfig(t, `
worker-port: 4321
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

	assert.Equal(t, 4321, c.WorkerPort)
	assert.Equal(t, "/srv/plex", c.PlexDir)
	assert.Equal(t, map[string]string{"FriendlyName": "Cluster Plex", "LogVerbose": "1"}, c.Preferences)
}

func TestConfigPrecedenceIsFlagThenEnvThenFile(t *testing.T) {
	podIdentity(t)
	path := writeConfig(t, "probe-port: 1111\nworker-port: 2222\npostgres-port: 3333\n")
	t.Setenv("CLUSTERPLEX_PROBE_PORT", "4444")
	t.Setenv("CLUSTERPLEX_WORKER_PORT", "5555")

	c, err := loadConfig([]string{"--config", path, "--probe-port", "6666"})
	require.NoError(t, err)

	assert.Equal(t, 6666, c.ProbePort, "flag wins")
	assert.Equal(t, 5555, c.WorkerPort, "env beats file")
	assert.Equal(t, 3333, c.Postgres.Port, "file beats default")
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
	c, err := loadConfig([]string{"--plex-preference", "FriendlyName=a=b"})
	require.NoError(t, err)
	assert.Equal(t, "a=b", c.Preferences["FriendlyName"])
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
	t.Setenv("CLUSTERPLEX_WORKER_PORT", "abc")
	_, err := loadConfig(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-port")
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

func TestPostgresSettingsComeFromTheEnvironmentLikeEverythingElse(t *testing.T) {
	podIdentity(t)
	t.Setenv("CLUSTERPLEX_POSTGRES_HOST", "db.media.svc")
	t.Setenv("CLUSTERPLEX_POSTGRES_PASSWORD", "s3cret")
	t.Setenv("CLUSTERPLEX_POSTGRES_DATABASE", "plexlib")

	c, err := loadConfig(nil)
	require.NoError(t, err)

	assert.Equal(t, "db.media.svc", c.Postgres.Host)
	assert.Equal(t, "plexlib", c.Postgres.Database)
	assert.Equal(t, "s3cret", c.Postgres.Password)
}

// The link is link-local and invisible outside the pod, but it still has to
// not collide with anything the pod already routes, so it stays configurable.
func TestPlexSubnetIsConfigurable(t *testing.T) {
	podIdentity(t)
	c, err := loadConfig([]string{"--plex-subnet", "10.255.0.0/30"})
	require.NoError(t, err)
	assert.Equal(t, "10.255.0.0/30", c.PlexSubnet.String())
}

func TestConfigRejectsAPlexSubnetThatIsNotAPrefix(t *testing.T) {
	podIdentity(t)
	_, err := loadConfig([]string{"--plex-subnet", "169.254.1.1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plex-subnet")
}
