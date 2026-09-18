package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestConfigDefaults(t *testing.T) {
	c, err := loadConfig(envFrom(map[string]string{"POD_NAME": "plex-0", "POD_NAMESPACE": "media"}))
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
}

func TestConfigReadsOverrides(t *testing.T) {
	c, err := loadConfig(envFrom(map[string]string{
		"POD_NAME": "plex-1", "POD_NAMESPACE": "media",
		"CLUSTERPLEX_PROXY_PORT": "1234",
		"CLUSTERPLEX_PMS_BINARY": "/opt/pms",
	}))
	require.NoError(t, err)
	assert.Equal(t, 1234, c.ProxyPort)
	assert.Equal(t, "/opt/pms", c.PMSBinary)
	assert.Equal(t, "plex-1", c.PodName)
}

func TestConfigRejectsMalformedPort(t *testing.T) {
	_, err := loadConfig(envFrom(map[string]string{"POD_NAME": "p", "POD_NAMESPACE": "n", "CLUSTERPLEX_PROXY_PORT": "abc"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CLUSTERPLEX_PROXY_PORT")
}

func TestConfigRequiresPodIdentity(t *testing.T) {
	_, err := loadConfig(envFrom(map[string]string{}))
	require.Error(t, err)
}
