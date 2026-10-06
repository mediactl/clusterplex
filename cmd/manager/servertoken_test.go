package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	plexprefs "github.com/mediactl/clusterplex/pkg/plex/prefs"
	"github.com/mediactl/clusterplex/pkg/plex/servertoken"
)

func tokenManager(t *testing.T, token string) *Manager {
	t.Helper()
	m := newTestManager()
	m.Config.PlexDir = t.TempDir()
	m.Config.Clustarr = ClustarrConfig{Enabled: true, Namespace: "clustarr-system", TokenSecret: "plex-token"}
	content := `<Preferences ProcessedMachineIdentifier="server-id"`
	if token != "" {
		content += ` PlexOnlineToken="` + token + `"`
	}
	require.NoError(t, os.WriteFile(filepath.Join(m.Config.PlexDir, "Preferences.xml"), []byte(content+`/>`), 0o600))
	return m
}

func secretToken(t *testing.T, m *Manager) string {
	t.Helper()
	sec, err := m.K8sClient.CoreV1().Secrets("clustarr-system").Get(context.Background(), "plex-token", metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return string(sec.Data[servertoken.TokenKey])
}

// Before Plex starts, a claim code is exchanged for the server's token and
// recorded in the Secret clustarr reads.
func TestAClaimIsExchangedBeforePlexStarts(t *testing.T) {
	m := tokenManager(t, "")
	m.Config.Claim = "claim-abc"
	m.claimExchange = func(_ context.Context, claim, clientID string) (string, error) {
		assert.Equal(t, "claim-abc", claim)
		assert.Equal(t, "server-id", clientID)
		return "server-token", nil
	}
	require.NoError(t, m.settleServerToken(t.Context()))

	got, err := plexprefs.Value(m.Config.PreferencesFile(), "PlexOnlineToken")
	require.NoError(t, err)
	assert.Equal(t, "server-token", got)
	assert.Equal(t, "server-token", secretToken(t, m))
}

// Without a token Secret the manager touches no Secret at all.
func TestNoTokenSecretWritesNoSecret(t *testing.T) {
	m := tokenManager(t, "server-token")
	m.Config.Clustarr.TokenSecret = ""
	require.NoError(t, m.settleServerToken(t.Context()))
	m.mirrorEvery = 10 * time.Millisecond
	m.takePlexTV(t.Context())
	defer m.stopLeaderWork()
	time.Sleep(50 * time.Millisecond)
	list, err := m.K8sClient.CoreV1().Secrets("clustarr-system").List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items)
}

// The lease holder keeps clustarr's Secret holding the file's token, and
// follows the file when the token changes there (a sign-in through Plex Web).
func TestTheLeaseHolderMirrorsTheTokenIntoClustarrsSecret(t *testing.T) {
	m := tokenManager(t, "server-token")
	m.mirrorEvery = 10 * time.Millisecond
	m.takePlexTV(t.Context())
	defer m.stopLeaderWork()
	require.Eventually(t, func() bool { return secretToken(t, m) == "server-token" }, 5*time.Second, 10*time.Millisecond)

	_, err := plexprefs.Apply(m.Config.PreferencesFile(), map[string]string{"PlexOnlineToken": "signed-in-again"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return secretToken(t, m) == "signed-in-again" }, 5*time.Second, 10*time.Millisecond)
}
